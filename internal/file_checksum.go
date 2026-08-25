package internal

import (
	"encoding/hex"
	"io"

	"github.com/cespare/xxhash/v2"
	"github.com/wal-g/wal-g/internal/ioextensions"
)

// ChecksumAlgoXXH64 is the name stored in the backup metadata alongside every checksum. It is
// recorded explicitly so that the algorithm can be changed later without invalidating backups
// made by older versions.
const ChecksumAlgoXXH64 = "xxh64"

// FileChecksummer computes a checksum of everything that is read through it. It is meant to be
// wrapped around the stream that is being packed into a tarball, so that the checksum comes for
// free without reading the file a second time. The checksum is only complete once the wrapped
// stream has been read to the end.
type FileChecksummer struct {
	digest *xxhash.Digest
}

func NewFileChecksummer() *FileChecksummer {
	return &FileChecksummer{digest: xxhash.New()}
}

// WrapReadCloser returns a ReadCloser that passes everything read from source through the
// checksummer. Closing the result closes source.
func (checksummer *FileChecksummer) WrapReadCloser(source io.ReadCloser) io.ReadCloser {
	return &ioextensions.ReadCascadeCloser{
		Reader: io.TeeReader(source, checksummer.digest),
		Closer: source,
	}
}

// Write makes FileChecksummer usable where bytes are fed in directly instead of being read
// through it, e.g. while scanning a paged file page by page.
func (checksummer *FileChecksummer) Write(p []byte) (int, error) {
	return checksummer.digest.Write(p)
}

// Checksum returns the hex encoded checksum of everything seen so far.
func (checksummer *FileChecksummer) Checksum() string {
	sum := checksummer.digest.Sum(nil)
	return hex.EncodeToString(sum)
}
