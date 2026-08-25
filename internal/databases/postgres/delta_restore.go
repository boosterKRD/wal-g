package postgres

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/utility"
)

const (
	postmasterPidFilename = "postmaster.pid"
	pgVersionFilename     = "PG_VERSION"
)

// PgRunningError is returned when the destination directory belongs to a running cluster.
type PgRunningError struct {
	error
}

func newPgRunningError(dbDataDirectory string) PgRunningError {
	return PgRunningError{errors.Errorf(
		"unable to restore while PostgreSQL is running: '%s' exists in %s.\n"+
			"Stop the cluster, and remove that file only if PostgreSQL is not running.\n",
		postmasterPidFilename, dbDataDirectory)}
}

// FetchOption tunes how a backup is restored. Options are variadic so that callers that do not
// care about them, such as the Greenplum segment fetcher, stay untouched.
type FetchOption func(*fetchOptions)

type fetchOptions struct {
	deltaRestore bool
}

// WithDeltaRestore keeps the files in the destination directory that already match the backup and
// only fetches the ones that differ, instead of requiring the directory to be empty.
func WithDeltaRestore(deltaRestore bool) FetchOption {
	return func(options *fetchOptions) {
		options.deltaRestore = deltaRestore
	}
}

func newFetchOptions(options []FetchOption) fetchOptions {
	var result fetchOptions
	for _, option := range options {
		option(&result)
	}
	return result
}

// DeltaRestoreStats sums up what the comparison against the destination directory decided.
type DeltaRestoreStats struct {
	Preserved      int
	Restored       int
	PreservedBytes int64
}

// ValidateDeltaRestoreTarget checks that a delta restore into dbDataDirectory is safe to attempt.
// It returns false when delta restore has to be turned off, in which case the restore falls back
// to the usual rules and will refuse to write into a directory that is not empty.
func ValidateDeltaRestoreTarget(dbDataDirectory string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dbDataDirectory, postmasterPidFilename)); err == nil {
		return false, newPgRunningError(dbDataDirectory)
	} else if !os.IsNotExist(err) {
		return false, err
	}

	isEmpty, err := utility.IsDirectoryEmpty(dbDataDirectory, nil)
	if err != nil {
		return false, err
	}
	if isEmpty {
		// Nothing to compare against, so a delta restore is just a restore. Turning it off here
		// keeps the destructive parts of it away from a directory that does not need them.
		tracelog.InfoLogger.Printf("Destination directory %s is empty, doing a regular restore", dbDataDirectory)
		return false, nil
	}

	// Refuse to treat a directory that does not look like PGDATA as one: a delta restore removes
	// and overwrites files, and getting the directory wrong would be expensive.
	if _, err := os.Stat(filepath.Join(dbDataDirectory, pgVersionFilename)); err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		tracelog.WarningLogger.Printf(
			"Delta restore requested, but no '%s' in %s to confirm that this is a valid PGDATA directory. "+
				"Delta restore has been disabled, and the restore will be aborted because the directory is not empty.",
			pgVersionFilename, dbDataDirectory)
		return false, nil
	}

	return true, nil
}

// prepareDeltaRestore checks that a delta restore can be done into dbDataDirectory and narrows
// filesToUnwrap down to the files that differ from the backup. The returned flag says whether
// delta restore is actually in effect; when it is not, the caller has to fall back to the usual
// rules and restore into an empty directory.
func prepareDeltaRestore(dbDataDirectory string, filesMeta FilesMetadataDto,
	filesToUnwrap map[string]bool) (map[string]bool, bool, error) {
	enabled, err := ValidateDeltaRestoreTarget(dbDataDirectory)
	if err != nil || !enabled {
		return filesToUnwrap, false, err
	}

	if !hasChecksums(filesMeta) {
		tracelog.WarningLogger.Println(
			"Delta restore requested, but the backup carries no file checksums. It was most likely taken by a " +
				"version of WAL-G without delta restore support. Delta restore has been disabled.")
		return filesToUnwrap, false, nil
	}

	// Remove pg_control up front so that a cluster left behind by an interrupted restore cannot be
	// started. It is restored last, once everything else is in place.
	if err := RemovePgControl(dbDataDirectory); err != nil {
		return nil, false, err
	}

	// Report what is in the directory but not in the backup before anything is written, so that
	// the list describes the state the operator actually has on disk.
	if _, err := LogExtraFiles(dbDataDirectory, filesMeta); err != nil {
		return nil, false, err
	}

	filesToUnwrap, _, err = SelectFilesToRestore(dbDataDirectory, filesMeta, filesToUnwrap)
	if err != nil {
		return nil, false, err
	}
	return filesToUnwrap, true, nil
}

// LogExtraFiles reports the files that are in dbDataDirectory but not in the backup. pgBackRest
// deletes them at this point, because they are changes the restored cluster diverged by. WAL-G
// only lists them for now, so that the list can be checked against real clusters before anything
// is removed automatically.
//
// Files under a directory that WAL-G does not back up, pg_wal for instance, are left out, and so
// are tablespaces, which live outside the data directory behind a symlink.
// It returns how many files it reported.
func LogExtraFiles(dbDataDirectory string, filesMeta FilesMetadataDto) (int, error) {
	if len(filesMeta.Files) == 0 {
		// Nothing to compare against, every file would look extra.
		return 0, nil
	}

	extraCount := 0
	err := filepath.Walk(dbDataDirectory, func(filePath string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		if _, excluded := ExcludedFilenames[fileInfo.Name()]; excluded {
			if fileInfo.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Only regular files are listed in the backup metadata, so only they can be told apart.
		if !fileInfo.Mode().IsRegular() {
			return nil
		}

		relativePath, err := filepath.Rel(dbDataDirectory, filePath)
		if err != nil {
			return err
		}
		name := "/" + filepath.ToSlash(relativePath)
		if _, inBackup := filesMeta.Files[name]; inBackup || UtilityFilePaths[name] {
			return nil
		}

		tracelog.InfoLogger.Printf("would remove invalid file '%s'", filePath)
		extraCount++
		return nil
	})
	if err != nil {
		return 0, err
	}

	if extraCount > 0 {
		tracelog.InfoLogger.Printf(
			"Delta restore: %d files are not part of the backup and are left in place. "+
				"They are changes this cluster diverged by, and pgBackRest would have removed them.", extraCount)
	}
	return extraCount, nil
}

func hasChecksums(filesMeta FilesMetadataDto) bool {
	for _, description := range filesMeta.Files {
		if description.Checksum != "" {
			return true
		}
	}
	return false
}

// SelectFilesToRestore drops from filesToUnwrap every file whose copy in dbDataDirectory already
// matches the backup, so that only the differing files are fetched. Files without a checksum in
// the backup metadata are always restored.
func SelectFilesToRestore(dbDataDirectory string, filesMeta FilesMetadataDto,
	filesToUnwrap map[string]bool) (map[string]bool, DeltaRestoreStats, error) {
	concurrency, err := conf.GetMaxDownloadConcurrency()
	if err != nil {
		return nil, DeltaRestoreStats{}, err
	}

	names := make([]string, 0, len(filesToUnwrap))
	for name := range filesToUnwrap {
		names = append(names, name)
	}

	var mutex sync.Mutex
	var stats DeltaRestoreStats
	result := make(map[string]bool, len(filesToUnwrap))

	// The cluster is stopped during a restore, so all the cores can be put to work hashing.
	work := make(chan string)
	var waitGroup sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for name := range work {
				preserve := matchesBackup(dbDataDirectory, name, filesMeta.Files[name])

				mutex.Lock()
				if preserve {
					stats.Preserved++
					stats.PreservedBytes += filesMeta.Files[name].Size
				} else {
					stats.Restored++
					result[name] = true
				}
				mutex.Unlock()
			}
		}()
	}
	for _, name := range names {
		work <- name
	}
	close(work)
	waitGroup.Wait()

	tracelog.InfoLogger.Printf("Delta restore: %d files match the backup and are kept (%d bytes), %d files will be restored",
		stats.Preserved, stats.PreservedBytes, stats.Restored)

	return result, stats, nil
}

// matchesBackup tells whether the local copy of a file can be kept as it is.
func matchesBackup(dbDataDirectory, name string, description internal.BackupFileDescription) bool {
	// Utility files are cheap and are restored in a specific order, never keep them.
	if UtilityFilePaths[name] {
		return false
	}
	if description.Checksum == "" || description.ChecksumAlgo != internal.ChecksumAlgoXXH64 {
		return false
	}

	filePath := path.Join(dbDataDirectory, name)
	fileInfo, err := os.Stat(filePath)
	if err != nil || fileInfo.IsDir() {
		return false
	}
	// A file of a different length cannot match, and this saves reading it.
	if fileInfo.Size() != description.Size {
		return false
	}
	if description.Size == 0 {
		tracelog.DebugLogger.Printf("restore file %s - exists and is zero size", filePath)
		return true
	}

	checksum, err := checksumLocalFile(filePath)
	if err != nil {
		tracelog.WarningLogger.Printf("Failed to checksum '%s', it will be restored: %v", filePath, err)
		return false
	}
	if checksum != description.Checksum {
		return false
	}

	tracelog.DebugLogger.Printf("restore file %s - exists and matches backup, checksum %s", filePath, checksum)
	restoreMTime(filePath, description)
	return true
}

// restoreMTime puts the modification time back to what it was in the backup, so that a restored
// cluster looks the same whether or not a file had to be fetched.
func restoreMTime(filePath string, description internal.BackupFileDescription) {
	if description.MTime.IsZero() {
		return
	}
	if err := os.Chtimes(filePath, description.MTime, description.MTime); err != nil {
		tracelog.WarningLogger.Printf("Failed to set modification time of '%s': %v", filePath, err)
	}
}

func checksumLocalFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer utility.LoggedClose(file, "")

	checksummer := internal.NewFileChecksummer()
	if _, err := io.Copy(checksummer, file); err != nil {
		return "", err
	}
	return checksummer.Checksum(), nil
}

// RemovePgControl deletes pg_control before a restore starts, so that a cluster left behind by an
// interrupted restore cannot be started. WAL-G restores pg_control last, after everything else.
func RemovePgControl(dbDataDirectory string) error {
	filePath := path.Join(dbDataDirectory, PgControlPath)
	err := os.Remove(filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	tracelog.InfoLogger.Printf("Removed '%s' so the cluster will not start if the restore does not complete", filePath)
	return nil
}
