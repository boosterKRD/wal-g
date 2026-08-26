package postgres

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wal-g/wal-g/internal"
)

// unwrapOneFile puts a single regular file through the interpreter, the way extraction does, and
// returns whatever it made of it.
func unwrapOneFile(t *testing.T, dir string, filesMeta FilesMetadataDto, name string, content []byte) error {
	t.Helper()

	interpreter := NewFileTarInterpreter(dir, BackupSentinelDto{}, filesMeta, nil, false)
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(content)),
		Mode:     0o600,
	}
	return interpreter.Interpret(bytes.NewReader(content), header)
}

func checksumOfBytes(t *testing.T, content []byte) string {
	t.Helper()

	checksummer := internal.NewFileChecksummer()
	_, err := io.Copy(checksummer, bytes.NewReader(content))
	require.NoError(t, err)
	return checksummer.Checksum()
}

func describeWithChecksum(t *testing.T, content []byte) internal.BackupFileDescription {
	t.Helper()

	return internal.BackupFileDescription{
		Size:         int64(len(content)),
		Checksum:     checksumOfBytes(t, content),
		ChecksumAlgo: internal.ChecksumAlgoXXH64,
	}
}

func TestRestoreChecksum_AcceptsAMatchingFile(t *testing.T) {
	content := []byte("the same bytes the backup was taken from")
	dir := t.TempDir()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/16384": describeWithChecksum(t, content),
	}}

	require.NoError(t, unwrapOneFile(t, dir, filesMeta, "/base/1/16384", content))

	written, err := os.ReadFile(filepath.Join(dir, "base", "1", "16384"))
	require.NoError(t, err)
	assert.Equal(t, content, written)
}

func TestRestoreChecksum_RejectsCorruptedContent(t *testing.T) {
	content := []byte("the same bytes the backup was taken from")
	corrupted := []byte("the same bytes the backup was taken FROM")
	dir := t.TempDir()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/16384": describeWithChecksum(t, content),
	}}

	err := unwrapOneFile(t, dir, filesMeta, "/base/1/16384", corrupted)
	require.Error(t, err)
	assert.IsType(t, ChecksumMismatchError{}, err)
	assert.Contains(t, err.Error(), "/base/1/16384")
}

// A stream that ends early leaves a short file. The size is reported alongside the checksum so
// that a truncated download does not read as an unexplained mismatch.
func TestRestoreChecksum_RejectsATruncatedFile(t *testing.T) {
	content := []byte("the same bytes the backup was taken from")
	dir := t.TempDir()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/16384": describeWithChecksum(t, content),
	}}

	err := unwrapOneFile(t, dir, filesMeta, "/base/1/16384", content[:10])
	require.Error(t, err)
	assert.IsType(t, ChecksumMismatchError{}, err)
	assert.Contains(t, err.Error(), "10")
}

// Backups taken before checksums were recorded carry none, and must restore as they always did.
func TestRestoreChecksum_SkipsFilesWithoutAChecksum(t *testing.T) {
	content := []byte("a file of an older backup")
	dir := t.TempDir()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/16384": internal.BackupFileDescription{},
	}}

	require.NoError(t, unwrapOneFile(t, dir, filesMeta, "/base/1/16384", content))

	written, err := os.ReadFile(filepath.Join(dir, "base", "1", "16384"))
	require.NoError(t, err)
	assert.Equal(t, content, written)
}

// A file the metadata says nothing about, backup_label for one, is written unchecked.
func TestRestoreChecksum_SkipsFilesThatAreNotInTheMetadata(t *testing.T) {
	content := []byte("START WAL LOCATION: 0/2000028")
	dir := t.TempDir()

	require.NoError(t, unwrapOneFile(t, dir, FilesMetadataDto{}, "/backup_label", content))

	written, err := os.ReadFile(filepath.Join(dir, "backup_label"))
	require.NoError(t, err)
	assert.Equal(t, content, written)
}

// The checksum of an incremented file describes the whole file, while the tar entry holds only the
// pages that changed. Checking one against the other would fail every time.
func TestRestoreChecksum_SkipsIncrementedFiles(t *testing.T) {
	dir := t.TempDir()

	description := describeWithChecksum(t, []byte("the whole file as it was on disk"))
	description.IsIncremented = true
	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{"/base/1/16384": description}}

	interpreter := NewFileTarInterpreter(dir, BackupSentinelDto{}, filesMeta, nil, false)

	verifier := interpreter.newChecksumVerifier("/base/1/16384", false)
	assert.NoError(t, verifier.verify())
}

// An algorithm this version cannot compute is a warning, not a failure: the file is restored, just
// without being checked.
func TestRestoreChecksum_SkipsAnUnknownAlgorithm(t *testing.T) {
	content := []byte("hashed by something else")
	dir := t.TempDir()

	filesMeta := FilesMetadataDto{Files: internal.BackupFileList{
		"/base/1/16384": {Size: int64(len(content)), Checksum: "abcdef", ChecksumAlgo: "sha256"},
	}}

	require.NoError(t, unwrapOneFile(t, dir, filesMeta, "/base/1/16384", content))

	written, err := os.ReadFile(filepath.Join(dir, "base", "1", "16384"))
	require.NoError(t, err)
	assert.Equal(t, content, written)
}
