package postgres

import (
	"archive/tar"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/internal/walparser"
)

// checksumOf returns the checksum the packer is expected to record for the given bytes.
func checksumOf(t *testing.T, data []byte) string {
	t.Helper()
	digest := xxhash.New()
	_, err := digest.Write(data)
	require.NoError(t, err)
	return hex.EncodeToString(digest.Sum(nil))
}

// packOneFile runs the packer over a single file and returns the description it recorded.
func packOneFile(t *testing.T, packer *TarBallFilePackerImpl, files internal.BundleFiles,
	path, name string, isIncremented bool) (internal.BackupFileDescription, bool) {
	t.Helper()

	fileInfo, err := os.Stat(path)
	require.NoError(t, err)

	header, err := tar.FileInfoHeader(fileInfo, fileInfo.Name())
	require.NoError(t, err)
	header.Name = name

	cfi := internal.NewComposeFileInfo(path, fileInfo, false, isIncremented, header)
	tarBall := internal.NewNopTarBallMaker().Make(false)

	require.NoError(t, packer.PackFileIntoTar(t.Context(), cfi, tarBall))

	rawDescription, ok := files.GetUnderlyingMap().Load(name)
	if !ok {
		return internal.BackupFileDescription{}, false
	}
	return rawDescription.(internal.BackupFileDescription), true
}

func TestPackFileIntoTar_ChecksumsRegularFile(t *testing.T) {
	content := []byte("wal-g delta restore checksum test")
	path := filepath.Join(t.TempDir(), "postgresql.conf")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	files := &internal.RegularBundleFiles{}
	packer := NewTarBallFilePacker(nil, nil, files, NewTarBallFilePackerOptions(false, false))
	packer.EnableChecksums()

	description, ok := packOneFile(t, packer, files, path, "/postgresql.conf", false)

	require.True(t, ok)
	assert.Equal(t, checksumOf(t, content), description.Checksum)
	assert.Equal(t, internal.ChecksumAlgoXXH64, description.ChecksumAlgo)
	assert.Equal(t, int64(len(content)), description.Size)
	assert.False(t, description.IsIncremented)
}

// Checksums are opt-in: Greenplum shares this packer and its backups must keep behaving as before.
func TestPackFileIntoTar_NoChecksumUnlessEnabled(t *testing.T) {
	content := []byte("wal-g delta restore checksum test")
	path := filepath.Join(t.TempDir(), "postgresql.conf")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	files := &internal.RegularBundleFiles{}
	packer := NewTarBallFilePacker(nil, nil, files, NewTarBallFilePackerOptions(false, false))

	description, ok := packOneFile(t, packer, files, path, "/postgresql.conf", false)

	require.True(t, ok)
	assert.Empty(t, description.Checksum)
	assert.Empty(t, description.ChecksumAlgo)
	assert.Zero(t, description.Size)
}

// An incremented file only sends its changed pages into the tarball, but a full scan reads the
// whole file, so the checksum must still describe the complete file.
func TestPackFileIntoTar_ChecksumsIncrementedFileOnFullScan(t *testing.T) {
	content, err := os.ReadFile(pagedFileName)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "16384")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	files := &internal.RegularBundleFiles{}
	incrementFromLsn := smallLSN
	// deltaMap is nil, so there is no bitmap and the page reader falls back to a full scan.
	packer := NewTarBallFilePacker(nil, &incrementFromLsn, files, NewTarBallFilePackerOptions(false, false))
	packer.EnableChecksums()

	description, ok := packOneFile(t, packer, files, path, "/base/1/16384", true)

	require.True(t, ok)
	assert.True(t, description.IsIncremented)
	assert.Equal(t, checksumOf(t, content), description.Checksum)
	assert.Equal(t, internal.ChecksumAlgoXXH64, description.ChecksumAlgo)
	assert.Equal(t, int64(len(content)), description.Size)
}

// With a WAL delta bitmap only the changed pages are read, so no checksum of the whole file can be
// produced and the description must be left without one.
func TestPackFileIntoTar_NoChecksumWithDeltaBitmap(t *testing.T) {
	content, err := os.ReadFile(pagedFileName)
	require.NoError(t, err)

	// The path has to look like a relation file, otherwise the delta map cannot resolve it.
	dbDir := filepath.Join(t.TempDir(), "base", "1")
	require.NoError(t, os.MkdirAll(dbDir, 0o700))
	path := filepath.Join(dbDir, "16384")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	deltaMap := NewPagedFileDeltaMap()
	relFileNode, err := GetRelFileNodeFrom(path)
	require.NoError(t, err)
	deltaMap.AddLocationsToDelta([]walparser.BlockLocation{
		*walparser.NewBlockLocation(relFileNode.SpcNode, relFileNode.DBNode, relFileNode.RelNode, 0),
	})

	files := &internal.RegularBundleFiles{}
	incrementFromLsn := smallLSN
	packer := NewTarBallFilePacker(deltaMap, &incrementFromLsn, files, NewTarBallFilePackerOptions(false, false))
	packer.EnableChecksums()

	description, ok := packOneFile(t, packer, files, path, "/base/1/16384", true)

	require.True(t, ok)
	assert.True(t, description.IsIncremented)
	assert.Empty(t, description.Checksum)
	assert.Empty(t, description.ChecksumAlgo)
}
