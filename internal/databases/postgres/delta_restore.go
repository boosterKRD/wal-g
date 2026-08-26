package postgres

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
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

	// Remove what is in the directory but not in the backup before anything is written. Doing it
	// first means the restore cannot delete a file it has just put in place, and the files removed
	// here do not have to be checksummed below.
	if _, err := RemoveExtraFiles(dbDataDirectory, filesMeta); err != nil {
		return nil, false, err
	}

	filesToUnwrap, _, err = SelectFilesToRestore(dbDataDirectory, filesMeta, filesToUnwrap)
	if err != nil {
		return nil, false, err
	}
	return filesToUnwrap, true, nil
}

// RemoveExtraFiles deletes everything in dbDataDirectory that the backup does not have. Those are
// the changes the cluster on disk diverged by, and leaving them behind would make the restored
// cluster a mixture of two different points in time. pgBackRest removes them at this point too.
//
// It runs before anything is written, so it can never delete a file the restore has just put in
// place. Directories WAL-G does not back up, pg_wal for instance, are left alone entirely: their
// contents were never copied, so the backup cannot bring them back.
// It returns how many entries it removed.
func RemoveExtraFiles(dbDataDirectory string, filesMeta FilesMetadataDto) (int, error) {
	if len(filesMeta.Files) == 0 {
		// Nothing to compare against, every file would look extra.
		return 0, nil
	}

	backupDirs := backupDirectories(filesMeta)

	removedCount, err := removeExtraFilesUnder(dbDataDirectory, "", filesMeta, backupDirs)
	if err != nil {
		return 0, err
	}

	tablespaceRemovedCount, err := removeExtraFilesInTablespaces(dbDataDirectory, filesMeta, backupDirs)
	if err != nil {
		return 0, err
	}
	removedCount += tablespaceRemovedCount

	if removedCount > 0 {
		tracelog.InfoLogger.Printf(
			"Delta restore: %d files and directories were not part of the backup and have been removed.",
			removedCount)
	}
	return removedCount, nil
}

// removeExtraFilesUnder walks one directory tree and removes what the backup does not have.
// namePrefix is what the paths under root are called in the backup metadata: empty for the data
// directory itself, and /pg_tblspc/<link> for a tablespace.
func removeExtraFilesUnder(root, namePrefix string, filesMeta FilesMetadataDto,
	backupDirs map[string]bool) (int, error) {
	removedCount := 0
	err := filepath.Walk(root, func(filePath string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		relativePath, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if relativePath == "." {
			// The root of the walk itself, which is the thing being restored into.
			return nil
		}
		// filepath.Walk does not follow symlinks, so it cannot wander outside root on its own.
		// The check is here so that a future change cannot turn this into a walk that deletes
		// somebody else's files.
		if !isUnder(root, filePath) {
			return errors.Errorf("delta restore: refusing to remove '%s', it is outside '%s'", filePath, root)
		}

		if _, excluded := ExcludedFilenames[fileInfo.Name()]; excluded {
			// An excluded directory is in the backup, but empty: its contents were never copied.
			// Removing them would destroy data the restore cannot put back.
			if fileInfo.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := namePrefix + "/" + filepath.ToSlash(relativePath)

		// The symlinks in pg_tblspc are not in the metadata. The backup keeps the tablespaces in
		// its tablespace spec instead and recreates the symlinks from there, so one missing from
		// filesMeta.Files says nothing about whether it belongs.
		if fileInfo.Mode()&os.ModeSymlink != 0 && path.Dir(name) == "/"+TablespaceFolder {
			return nil
		}

		if _, inBackup := filesMeta.Files[name]; inBackup || UtilityFilePaths[name] {
			return nil
		}

		if fileInfo.IsDir() {
			if backupDirs[name] {
				// The backup has files under this directory even though the directory itself has
				// no entry of its own. Removing it would take them along.
				return nil
			}
			// Removing the directory takes everything inside it along, so the walk must not
			// descend into what is no longer there.
			if err := os.RemoveAll(filePath); err != nil {
				return errors.Wrapf(err, "delta restore: failed to remove directory '%s'", filePath)
			}
			tracelog.InfoLogger.Printf("remove invalid directory '%s'", filePath)
			removedCount++
			return filepath.SkipDir
		}

		// Everything else goes the same way: regular files, symlinks, and the sockets and fifos a
		// data directory should not contain at all. os.Remove drops a symlink itself rather than
		// what it points at.
		if err := os.Remove(filePath); err != nil {
			return errors.Wrapf(err, "delta restore: failed to remove '%s'", filePath)
		}
		tracelog.InfoLogger.Printf("remove invalid file '%s'", filePath)
		removedCount++
		return nil
	})
	return removedCount, err
}

// removeExtraFilesInTablespaces does the same for the tablespaces of the cluster. They live outside
// the data directory, reachable only through the symlinks in pg_tblspc, and filepath.Walk does not
// follow symlinks. The symlinks of the directory being restored into are followed rather than the
// locations recorded in the backup, because the question is what is on disk right now.
func removeExtraFilesInTablespaces(dbDataDirectory string, filesMeta FilesMetadataDto,
	backupDirs map[string]bool) (int, error) {
	tablespaceRoot := filepath.Join(dbDataDirectory, TablespaceFolder)
	entries, err := os.ReadDir(tablespaceRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	removedCount := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}

		location, err := os.Readlink(filepath.Join(tablespaceRoot, entry.Name()))
		if err != nil {
			tracelog.WarningLogger.Printf("Failed to read the tablespace symlink '%s': %v", entry.Name(), err)
			continue
		}

		count, err := removeExtraFilesUnder(location, "/"+TablespaceFolder+"/"+entry.Name(), filesMeta, backupDirs)
		if err != nil {
			return 0, err
		}
		removedCount += count
	}
	return removedCount, nil
}

// backupDirectories collects every directory that has something of the backup under it. Backups do
// carry an entry for each directory of their own, but relying on that alone would mean a single
// missing entry costs the whole subtree underneath it. A directory is only removed when the backup
// has nothing in it at all.
func backupDirectories(filesMeta FilesMetadataDto) map[string]bool {
	backupDirs := make(map[string]bool)

	addAncestors := func(name string) {
		for dir := path.Dir(name); dir != "/" && dir != "." && !backupDirs[dir]; dir = path.Dir(dir) {
			backupDirs[dir] = true
		}
	}

	for name := range filesMeta.Files {
		addAncestors(name)
	}
	// pg_control and the label files are restored separately and are not in filesMeta.Files, but
	// the directories holding them are as much a part of the backup as any other.
	for name := range UtilityFilePaths {
		addAncestors(name)
	}
	return backupDirs
}

// isUnder reports whether filePath is inside root. Both are expected to come from the same walk,
// so neither is resolved any further.
func isUnder(root, filePath string) bool {
	relativePath, err := filepath.Rel(root, filePath)
	if err != nil {
		return false
	}
	return relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator))
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
				if !preserve {
					removeStaleLocalFile(dbDataDirectory, name)
				}

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

// removeStaleLocalFile drops the local copy of a file that is about to be restored, so that the
// restore writes it from scratch.
//
// Without this, the reverse unpack implementation takes the file for one it may reuse: it calls
// RestoreMissingPages, which fills in the pages the local copy is missing and leaves the pages it
// already has alone. Those are exactly the stale pages the checksum said not to trust. The other
// implementation opens the file with O_TRUNC and does not have the problem, but it costs nothing
// to make both start from the same place.
//
// Only regular files of the backup are removed. Directories are recreated in place, and whatever
// the walk in RemoveExtraFiles left alone is left alone here too.
func removeStaleLocalFile(dbDataDirectory, name string) {
	filePath := path.Join(dbDataDirectory, name)

	// Directories are recreated in place and links are left to the restore, so only regular files
	// are removed here. A file that is not there needs no removing either.
	fileInfo, err := os.Lstat(filePath)
	if err != nil || !fileInfo.Mode().IsRegular() {
		return
	}
	if err := os.Remove(filePath); err != nil {
		tracelog.WarningLogger.Printf("Failed to remove '%s' before restoring it: %v", filePath, err)
	}
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
