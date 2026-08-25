package postgres_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/internal/databases/postgres"
	"github.com/wal-g/wal-g/testtools"
)

// composeFile writes a file of the given size and returns the ComposeFileInfo for it.
func composeFile(t *testing.T, dir, name string, size int) *internal.ComposeFileInfo {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, make([]byte, size), 0o600))

	fileInfo, err := os.Stat(path)
	require.NoError(t, err)

	header, err := tar.FileInfoHeader(fileInfo, fileInfo.Name())
	require.NoError(t, err)
	header.Name = "/" + name

	return internal.NewComposeFileInfo(path, fileInfo, false, false, header)
}

// tarBallOf returns the name of the tarball holding the given file.
func tarBallOf(t *testing.T, sets map[string][]string, fileName string) string {
	t.Helper()

	for tarBallName, fileNames := range sets {
		for _, name := range fileNames {
			if name == fileName {
				return tarBallName
			}
		}
	}
	t.Fatalf("file %s is not in any tarball", fileName)
	return ""
}

// A file at or above WALG_TAR_DEDICATED_FILE_SIZE must end up in a tarball of its own, so that
// delta restore can skip or fetch it without dragging unrelated files along.
func TestRegularTarBallComposer_LargeFileGetsItsOwnTarBall(t *testing.T) {
	internal.ConfigureSettings(conf.PG)
	conf.InitConfig()
	conf.Configure()
	// Set before the queue is started: the threshold is resolved in NewTarBallQueue.
	viper.Set(conf.TarDedicatedFileSizeSetting, 1024)
	viper.Set(conf.UploadDiskConcurrencySetting, 1)

	dir := t.TempDir()
	smallBefore := composeFile(t, dir, "small_before", 16)
	large := composeFile(t, dir, "large", 4096)
	smallAfter := composeFile(t, dir, "small_after", 16)

	bundle := &postgres.Bundle{
		Bundle: internal.Bundle{
			Directory:        dir,
			TarSizeThreshold: 1 << 20,
		},
	}
	uploader := testtools.NewMockUploader(false, false)
	require.NoError(t, bundle.StartQueue(internal.NewStorageTarBallMaker("mockBackup", uploader)))

	files := &internal.RegularBundleFiles{}
	tarFileSets := internal.NewRegularTarFileSets()
	composerMaker := postgres.NewRegularTarBallComposerMaker(
		postgres.NewTarBallFilePackerOptions(false, false), files, tarFileSets)
	composer, err := composerMaker.Make(t.Context(), bundle)
	require.NoError(t, err)

	composer.AddFile(smallBefore)
	composer.AddFile(large)
	composer.AddFile(smallAfter)

	sets, err := composer.FinishComposing()
	require.NoError(t, err)
	require.NoError(t, bundle.TarBallQueue.FinishQueue())

	fileSets := sets.Get()
	largeTarBall := tarBallOf(t, fileSets, "/large")

	assert.Equal(t, []string{"/large"}, fileSets[largeTarBall],
		"a large file must not share its tarball with anything else")
	assert.NotEqual(t, largeTarBall, tarBallOf(t, fileSets, "/small_before"))
	assert.NotEqual(t, largeTarBall, tarBallOf(t, fileSets, "/small_after"))
}

// Without the threshold reached, files keep sharing tarballs as they did before.
func TestRegularTarBallComposer_SmallFilesShareTarBall(t *testing.T) {
	internal.ConfigureSettings(conf.PG)
	conf.InitConfig()
	conf.Configure()
	viper.Set(conf.TarDedicatedFileSizeSetting, 1<<20)
	viper.Set(conf.UploadDiskConcurrencySetting, 1)

	dir := t.TempDir()
	first := composeFile(t, dir, "first", 16)
	second := composeFile(t, dir, "second", 16)

	bundle := &postgres.Bundle{
		Bundle: internal.Bundle{
			Directory:        dir,
			TarSizeThreshold: 1 << 20,
		},
	}
	uploader := testtools.NewMockUploader(false, false)
	require.NoError(t, bundle.StartQueue(internal.NewStorageTarBallMaker("mockBackup", uploader)))

	files := &internal.RegularBundleFiles{}
	tarFileSets := internal.NewRegularTarFileSets()
	composerMaker := postgres.NewRegularTarBallComposerMaker(
		postgres.NewTarBallFilePackerOptions(false, false), files, tarFileSets)
	composer, err := composerMaker.Make(t.Context(), bundle)
	require.NoError(t, err)

	composer.AddFile(first)
	composer.AddFile(second)

	sets, err := composer.FinishComposing()
	require.NoError(t, err)
	require.NoError(t, bundle.TarBallQueue.FinishQueue())

	fileSets := sets.Get()
	assert.Equal(t, tarBallOf(t, fileSets, "/first"), tarBallOf(t, fileSets, "/second"))
}
