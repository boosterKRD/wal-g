package postgres

import (
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

func TestSelectFilesToRestore(t *testing.T) {
	internal.ConfigureSettings(conf.PG)
	conf.InitConfig()
	conf.Configure()

	matching := []byte("this file did not change")
	diverged := []byte("this file did change")

	dir := writePgData(t, map[string][]byte{
		"base/1/matching":   matching,
		"base/1/diverged":   []byte("something else entirely"),
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
