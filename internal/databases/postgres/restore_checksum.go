package postgres

import (
	"io"

	"github.com/pkg/errors"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
)

// ChecksumMismatchError is returned when a file that has just been written does not match the
// checksum the backup recorded for it. The restore stops at that point: carrying on would only
// build more of a data directory that is already known to be wrong.
type ChecksumMismatchError struct {
	error
}

// UnretryableExtraction marks the error as one that downloading the tarball again cannot fix, so
// that extraction stops instead of working through its retries.
func (ChecksumMismatchError) UnretryableExtraction() {}

func newChecksumMismatchError(name, expected, actual string, expectedSize, actualSize int64) ChecksumMismatchError {
	return ChecksumMismatchError{errors.Errorf(
		"restored file '%s' does not match the backup: checksum %s, expected %s (%d bytes, expected %d).\n"+
			"The backup is damaged, or the data was corrupted on its way out of storage.\n",
		name, actual, expected, actualSize, expectedSize)}
}

// restoreChecksumVerifier checks a file against the checksum stored in the backup metadata while it
// is being written. The bytes are hashed as they pass by, so no second read of anything is needed.
//
// The zero value verifies nothing, which is what every case that cannot be checked gets.
type restoreChecksumVerifier struct {
	name         string
	expected     string
	expectedSize int64
	checksummer  *internal.FileChecksummer
	readSize     int64
}

// newChecksumVerifier prepares the check for one file of the backup. applicable says whether what
// is about to be written is the whole file: the checksum describes the file as it was on disk when
// the backup was taken, so anything that writes only a part of it cannot be checked this way.
func (tarInterpreter *FileTarInterpreter) newChecksumVerifier(name string, applicable bool) *restoreChecksumVerifier {
	if !applicable {
		return &restoreChecksumVerifier{}
	}

	description, ok := tarInterpreter.FilesMetadata.Files[name]
	if !ok || description.Checksum == "" {
		// Backups taken before checksums were recorded, and files of a backup taken with
		// WALG_WITHOUT_FILES_METADATA, carry nothing to check against.
		return &restoreChecksumVerifier{}
	}
	if description.ChecksumAlgo != internal.ChecksumAlgoXXH64 {
		tracelog.WarningLogger.Printf(
			"File '%s' carries a '%s' checksum, which this version of WAL-G cannot compute. "+
				"The file is restored without being verified.", name, description.ChecksumAlgo)
		return &restoreChecksumVerifier{}
	}

	return &restoreChecksumVerifier{
		name:         name,
		expected:     description.Checksum,
		expectedSize: description.Size,
		checksummer:  internal.NewFileChecksummer(),
	}
}

// wrap returns a reader that feeds the checksummer everything read through it.
func (verifier *restoreChecksumVerifier) wrap(fileReader io.Reader) io.Reader {
	if verifier.checksummer == nil {
		return fileReader
	}
	return io.TeeReader(fileReader, verifier)
}

// Write counts the bytes on their way into the checksummer, so that a stream that ended early is
// reported as the truncation it is rather than as an unexplained checksum mismatch.
func (verifier *restoreChecksumVerifier) Write(p []byte) (int, error) {
	verifier.readSize += int64(len(p))
	return verifier.checksummer.Write(p)
}

// verify is called once the file has been written. It reports a mismatch as an error, so that a
// restore never quietly leaves a damaged data directory behind.
func (verifier *restoreChecksumVerifier) verify() error {
	if verifier.checksummer == nil {
		return nil
	}

	actual := verifier.checksummer.Checksum()
	if actual == verifier.expected && verifier.readSize == verifier.expectedSize {
		return nil
	}
	return newChecksumMismatchError(verifier.name, verifier.expected, actual,
		verifier.expectedSize, verifier.readSize)
}
