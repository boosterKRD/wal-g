package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wal-g/wal-g/internal"
)

func earlyStopInterpreter(t *testing.T, filesToUnwrap map[string]bool) *FileTarInterpreter {
	t.Helper()

	filesMetadata := FilesMetadataDto{
		Files: internal.BackupFileList{
			"/base/1/16384": {},
			"/base/1/16385": {},
			"/base/1/16386": {},
		},
		TarFileSets: map[string][]string{
			"part_001.tar.lz4": {"/base/1", "/base/1/16384", "/base/1/16385", "/base/1/16386"},
		},
	}

	interpreter := NewFileTarInterpreter(t.TempDir(), BackupSentinelDto{}, filesMetadata, filesToUnwrap, false)
	interpreter.EnableEarlyStop()
	return interpreter
}

func TestInterestingEntryCount(t *testing.T) {
	t.Run("counts the wanted files and every non-file entry", func(t *testing.T) {
		interpreter := earlyStopInterpreter(t, map[string]bool{"/base/1/16385": true})

		count, known := interpreter.InterestingEntryCount("part_001.tar.lz4")

		assert.True(t, known)
		// The wanted file plus the directory entry, which is always created.
		assert.Equal(t, 2, count)
	})

	t.Run("is unknown for a tarball that is not in the file sets", func(t *testing.T) {
		interpreter := earlyStopInterpreter(t, map[string]bool{"/base/1/16385": true})

		_, known := interpreter.InterestingEntryCount("part_002.tar.lz4")

		assert.False(t, known)
	})

	t.Run("is unknown while early stop is off", func(t *testing.T) {
		interpreter := earlyStopInterpreter(t, map[string]bool{"/base/1/16385": true})
		interpreter.earlyStop = false

		_, known := interpreter.InterestingEntryCount("part_001.tar.lz4")

		assert.False(t, known)
	})

	// Without the file sets there is no way to know what a tarball holds, so it has to be read
	// to the end. This is the case for WALG_WITHOUT_FILES_METADATA backups.
	t.Run("is unknown without tar file sets", func(t *testing.T) {
		interpreter := NewFileTarInterpreter(t.TempDir(), BackupSentinelDto{}, FilesMetadataDto{},
			map[string]bool{"/base/1/16385": true}, false)
		interpreter.EnableEarlyStop()

		_, known := interpreter.InterestingEntryCount("part_001.tar.lz4")

		assert.False(t, known)
	})
}

func TestIsInterestingEntry(t *testing.T) {
	interpreter := earlyStopInterpreter(t, map[string]bool{"/base/1/16385": true})

	assert.True(t, interpreter.IsInterestingEntry("/base/1/16385"), "a wanted file is extracted")
	assert.False(t, interpreter.IsInterestingEntry("/base/1/16384"), "a file that is kept is skipped")
	assert.True(t, interpreter.IsInterestingEntry("/base/1"),
		"a directory is not a backup file and is always created")
}
