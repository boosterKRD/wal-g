package binary

import (
	"context"
	"strings"

	"github.com/wal-g/wal-g/internal"
)

const (
	CollectionPrefix = "collection"
	IndexPrefix      = "index"
)

type DirDatabaseTarBallComposerMaker struct {
	files       internal.BundleFiles
	tarFileSets internal.TarFileSets
}

func NewDirDatabaseTarBallComposerMaker() *DirDatabaseTarBallComposerMaker {
	return &DirDatabaseTarBallComposerMaker{
		files:       &internal.RegularBundleFiles{},
		tarFileSets: internal.NewRegularTarFileSets(),
	}
}

func (maker *DirDatabaseTarBallComposerMaker) Make(ctx context.Context, bundle *internal.Bundle) (internal.TarBallComposer, error) {
	packer := internal.NewRegularTarBallFilePacker(maker.files, false)
	return internal.NewDirDatabaseTarBallComposer(
		ctx,
		maker.files,
		bundle.TarBallQueue,
		packer,
		maker.tarFileSets,
		bundle.Crypter,
		mongoPathFilter,
		// Dedicated tarballs for large files exist for PostgreSQL delta restore; leave the layout
		// of mongo backups as it was.
		false,
	), nil
}

func mongoPathFilter(path string) bool {
	return strings.Contains(path, CollectionPrefix) || strings.Contains(path, IndexPrefix)
}
