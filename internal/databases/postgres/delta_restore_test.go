package postgres

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
)

func writePgData(t *testing.T, files map[string][]byte) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, content, 0o600))
	}
	return dir
}

func describe(t *testing.T, content []byte) internal.BackupFileDescription {
	t.Helper()

	checksummer := internal.NewFileChecksummer()
	_, err := checksummer.Write(content)
	require.NoError(t, err)

	return internal.BackupFileDescription{
		Size:         int64(len(content)),
		Checksum:     checksummer.Checksum(),
		ChecksumAlgo: internal.ChecksumAlgoXXH64,
		MTime:        time.Unix(1600000000, 0),
	}
}

func TestValidateDeltaRestoreTarget(t *testing.T) {
	t.Run("refuses a running cluster", func(t *testing.T) {
		dir := writePgData(t, map[string][]byte{
			"PG_VERSION":          []byte("17\n"),
			postmasterPidFilename: []byte("1234\n"),
		})

		_, err := ValidateDeltaRestoreTarget(dir)

		require.Error(t, err)
		assert.IsType(t, PgRunningError{}, err)
	})

	t.Run("turns delta off for an empty directory", func(t *testing.T) {
		enabled, err := ValidateDeltaRestoreTarget(t.TempDir())

		require.NoError(t, err)
		assert.False(t, enabled)
	})

	t.Run("turns delta off when the directory is not PGDATA", func(t *testing.T) {
		dir := writePgData(t, map[string][]byte{"some_file": []byte("data")})

		enabled, err := ValidateDeltaRestoreTarget(dir)

		require.NoError(t, err)
		assert.False(t, enabled)
	})

	t.Run("allows a stopped cluster", func(t *testing.T) {
		dir := writePgData(t, map[string][]byte{"PG_VERSION": []byte("17\n")})

		enabled, err := ValidateDeltaRestoreTarget(dir)

		require.NoError(t, err)
		assert.True(t, enabled)
	})
}

// The summary must not fall over on an empty run, and must say what it counted.
func TestDeltaRestoreStats_LogSummary(t *testing.T) {
	stats, ctx := NewDeltaRestoreStats(context.Background())
	require.NotNil(t, stats.Extraction)
	assert.Same(t, stats.Extraction, internal.ExtractionStatsFromContext(ctx))

	stats.FilesInBackup, stats.FilesInBackupBytes = 3, 3000
	stats.Preserved, stats.PreservedBytes = 2, 2000
	stats.Restored, stats.RestoredBytes = 1, 1000
	stats.Removed = 4
	stats.Extraction.AddTarball(500, false)
	stats.Extraction.AddTarball(500, true)
	stats.Extraction.AddRead(500, 100, true)

	// Only that it does not panic: the output goes to the logger.
	stats.LogSummary()
}

func TestSelectFilesToRestore(t *testing.T) {
	internal.ConfigureSettings(conf.PG)
	conf.InitConfig()
	conf.Configure()

	matching := []byte("this file did not change")
	diverged := []byte("this file did change")

	dir := writePgData(t, map[string][]byte{
		"base/1/matching":   matching,
		"base/1/diverged":   []byte("this file DID change"),
		"base/1/truncated":  matching[:4],
		"base/1/empty":      {},
		"base/1/no_hash":    matching,
		"global/pg_control": []byte("control"),
	})

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/matching":  describe(t, matching),
		"/base/1/diverged":  describe(t, diverged),
		"/base/1/truncated": describe(t, matching),
		"/base/1/empty":     {Size: 0, Checksum: "0", ChecksumAlgo: internal.ChecksumAlgoXXH64},
		"/base/1/no_hash":   {Size: int64(len(matching))},
		"/base/1/missing":   describe(t, matching),
		PgControlPath:       describe(t, []byte("control")),
	}}

	filesToUnwrap := map[string]bool{}
	for name := range filesMeta.Files {
		filesToUnwrap[name] = true
	}

	selected, stats, err := SelectFilesToRestore(dir, filesMeta, filesToUnwrap)
	require.NoError(t, err)

	assert.NotContains(t, selected, "/base/1/matching", "an unchanged file must be kept")
	assert.NotContains(t, selected, "/base/1/empty", "a zero length file must be kept")

	assert.Contains(t, selected, "/base/1/diverged", "a changed file must be restored")
	assert.Contains(t, selected, "/base/1/truncated", "a file of a different size must be restored")
	assert.Contains(t, selected, "/base/1/no_hash", "a file without a checksum must be restored")
	assert.Contains(t, selected, "/base/1/missing", "a file that is not there must be restored")
	assert.Contains(t, selected, PgControlPath, "pg_control must always be restored")

	assert.Equal(t, 2, stats.Preserved)
	assert.Equal(t, 5, stats.Restored)
	assert.Equal(t, int64(len(matching)), stats.PreservedBytes)

	var restoredBytes int64
	for name := range selected {
		restoredBytes += filesMeta.Files[name].Size
	}
	assert.Equal(t, restoredBytes, stats.RestoredBytes)

	// Only files of the right length get hashed: the matching one, and the diverged one, which is
	// the same length but different. The rest were decided without reading them.
	assert.Equal(t, 2, stats.LocallyRead)
	assert.Equal(t, int64(len(matching)+len(diverged)), stats.LocallyReadBytes)
}

// A file that is kept gets the modification time it had in the backup, so that the restored
// cluster looks the same whether or not the file had to be fetched.
func TestSelectFilesToRestore_RestoresMTimeOfKeptFiles(t *testing.T) {
	internal.ConfigureSettings(conf.PG)
	conf.InitConfig()
	conf.Configure()

	content := []byte("this file did not change")
	dir := writePgData(t, map[string][]byte{"base/1/matching": content})
	description := describe(t, content)

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{"/base/1/matching": description}}
	selected, _, err := SelectFilesToRestore(dir, filesMeta, map[string]bool{"/base/1/matching": true})
	require.NoError(t, err)
	require.Empty(t, selected)

	fileInfo, err := os.Stat(filepath.Join(dir, "base/1/matching"))
	require.NoError(t, err)
	assert.True(t, description.MTime.Equal(fileInfo.ModTime()))
}

func TestRemovePgControl(t *testing.T) {
	dir := writePgData(t, map[string][]byte{"global/pg_control": []byte("control")})

	require.NoError(t, RemovePgControl(dir))
	_, err := os.Stat(filepath.Join(dir, "global", "pg_control"))
	assert.True(t, os.IsNotExist(err))

	// Removing it again is not an error: an interrupted restore may have taken it already.
	assert.NoError(t, RemovePgControl(dir))
}

func TestRemoveExtraFiles(t *testing.T) {
	content := []byte("data")

	dir := writePgData(t, map[string][]byte{
		"PG_VERSION":                      []byte("17\n"),
		"base/1/16384":                    content,
		"base/1/leftover":                 content,
		"base/9999/16384":                 content,
		"pg_wal/000000010000000000000001": content,
		"pg_stat_tmp/global.stat":         content,
		"pg_stat/global.stat":             content,
		"global/pg_control":               content,
	})
	// An empty directory the backup does not have, and one it does.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "leftover_dir", "nested"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pg_twophase"), 0o700))
	// A symlink and a socket, neither of which belongs in a data directory.
	require.NoError(t, os.Symlink(filepath.Join(dir, "PG_VERSION"), filepath.Join(dir, "leftover_link")))
	listener, err := net.Listen("unix", filepath.Join(dir, "s.PGSQL.5432"))
	require.NoError(t, err)
	defer listener.Close()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/PG_VERSION":   describe(t, []byte("17\n")),
		"/base/1/16384": describe(t, content),
		"/pg_twophase":  describe(t, nil),
	}}

	removedCount, err := RemoveExtraFiles(dir, filesMeta)
	require.NoError(t, err)

	// base/1/leftover, base/9999 as a whole, pg_stat/global.stat, leftover_dir as a whole,
	// leftover_link and the socket.
	assert.Equal(t, 6, removedCount)

	for _, name := range []string{
		"base/1/leftover",
		"base/9999",
		"pg_stat/global.stat",
		"leftover_dir",
		"leftover_link",
		"s.PGSQL.5432",
	} {
		_, err := os.Lstat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(err), "%s must have been removed", name)
	}

	for _, name := range []string{
		"PG_VERSION",
		"base/1/16384",
		"pg_twophase",
		// Never backed up, so the backup cannot put them back either.
		"pg_wal/000000010000000000000001",
		"pg_stat_tmp/global.stat",
		// Restored separately, at the very end.
		"global/pg_control",
	} {
		_, err := os.Lstat(filepath.Join(dir, name))
		assert.NoError(t, err, "%s must be left in place", name)
	}
}

// A directory of the backup that has no entry of its own must survive on the strength of the files
// underneath it, or one missing entry would cost the whole subtree.
func TestRemoveExtraFiles_KeepsDirectoriesHoldingBackupFiles(t *testing.T) {
	content := []byte("data")

	dir := writePgData(t, map[string][]byte{
		"PG_VERSION":   []byte("17\n"),
		"base/1/16384": content,
	})

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/PG_VERSION":   describe(t, []byte("17\n")),
		"/base/1/16384": describe(t, content),
	}}

	removedCount, err := RemoveExtraFiles(dir, filesMeta)
	require.NoError(t, err)
	assert.Zero(t, removedCount)

	_, err = os.Stat(filepath.Join(dir, "base", "1", "16384"))
	assert.NoError(t, err)
}

func TestRemoveExtraFiles_NoMetadata(t *testing.T) {
	dir := writePgData(t, map[string][]byte{"base/1/16384": []byte("data")})

	// Without file metadata there is nothing to compare against, so nothing is removed.
	removedCount, err := RemoveExtraFiles(dir, FilesMetadataDto{})
	require.NoError(t, err)
	assert.Zero(t, removedCount)

	_, err = os.Stat(filepath.Join(dir, "base", "1", "16384"))
	assert.NoError(t, err)
}

// Tablespaces live outside the data directory, reachable only through the symlinks in pg_tblspc,
// so they need a walk of their own.
func TestRemoveExtraFiles_WalksTablespaces(t *testing.T) {
	content := []byte("data")

	dir := writePgData(t, map[string][]byte{
		"PG_VERSION": []byte("17\n"),
	})

	// A tablespace: a directory somewhere else, and a symlink to it under pg_tblspc.
	tablespaceDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tablespaceDir, "PG_17_202406281", "16384"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tablespaceDir, "PG_17_202406281", "16384", "1259"), content, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tablespaceDir, "PG_17_202406281", "16384", "leftover"), content, 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, TablespaceFolder), 0o700))
	require.NoError(t, os.Symlink(tablespaceDir, filepath.Join(dir, TablespaceFolder, "16385")))

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/PG_VERSION": describe(t, []byte("17\n")),
		"/" + TablespaceFolder + "/16385/PG_17_202406281/16384/1259": describe(t, content),
	}}

	removedCount, err := RemoveExtraFiles(dir, filesMeta)
	require.NoError(t, err)

	// Only the leftover inside the tablespace.
	assert.Equal(t, 1, removedCount)

	_, err = os.Stat(filepath.Join(tablespaceDir, "PG_17_202406281", "16384", "leftover"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(tablespaceDir, "PG_17_202406281", "16384", "1259"))
	assert.NoError(t, err)
}

// The symlinks in pg_tblspc are not in the file metadata: the backup keeps the tablespaces in its
// tablespace spec and recreates the symlinks from there. Removing one would leave the restored
// cluster without its tablespace.
func TestRemoveExtraFiles_KeepsTablespaceSymlinks(t *testing.T) {
	dir := writePgData(t, map[string][]byte{"PG_VERSION": []byte("17\n")})

	tablespaceDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, TablespaceFolder), 0o700))
	symlinkPath := filepath.Join(dir, TablespaceFolder, "16385")
	require.NoError(t, os.Symlink(tablespaceDir, symlinkPath))

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/PG_VERSION":          describe(t, []byte("17\n")),
		"/" + TablespaceFolder: describe(t, nil),
	}}

	removedCount, err := RemoveExtraFiles(dir, filesMeta)
	require.NoError(t, err)
	assert.Zero(t, removedCount)

	_, err = os.Lstat(symlinkPath)
	assert.NoError(t, err)
}

func TestRemoveExtraFiles_BrokenTablespaceSymlink(t *testing.T) {
	dir := writePgData(t, map[string][]byte{"PG_VERSION": []byte("17\n")})

	require.NoError(t, os.MkdirAll(filepath.Join(dir, TablespaceFolder), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, TablespaceFolder, "16385")))

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/PG_VERSION":          describe(t, []byte("17\n")),
		"/" + TablespaceFolder: describe(t, nil),
	}}

	// A symlink to a directory that is not there is not an error: nothing to remove.
	removedCount, err := RemoveExtraFiles(dir, filesMeta)
	require.NoError(t, err)
	assert.Zero(t, removedCount)
}
