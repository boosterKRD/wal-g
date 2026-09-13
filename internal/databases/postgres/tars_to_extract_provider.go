package postgres

import (
	"context"
	"regexp"

	"github.com/pkg/errors"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

type FilesToExtractProvider interface {
	Get(ctx context.Context, backup Backup, filesToUnwrap map[string]bool, skipRedundantTars bool) (
		tarsToExtract []internal.ReaderMaker, pgControlKey string, err error)
}

type FilesToExtractProviderImpl struct {
}

func (t FilesToExtractProviderImpl) Get(ctx context.Context, backup Backup, filesToUnwrap map[string]bool, skipRedundantTars bool) (
	concurrentTarsToExtract []internal.ReaderMaker, sequentialTarsToExtract []internal.ReaderMaker, err error) {
	_, filesMeta, err := backup.GetSentinelAndFilesMetadata(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The listing is needed for the names anyway; the sizes that come with it are what lets the
	// extraction stats say how much of the backup was never downloaded.
	tarPartitionFolder := backup.GetTarPartitionFolder()
	tarObjects, _, err := tarPartitionFolder.ListFolder(ctx)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "unable to list the tarballs of backup '%s'", backup.Name)
	}
	tracelog.DebugLogger.Printf("Tars to extract: '%+v'\n", tarObjectNames(tarObjects))
	concurrentTarsToExtract = make([]internal.ReaderMaker, 0, len(tarObjects))
	sequentialTarsToExtract = make([]internal.ReaderMaker, 0, 2)
	stats := internal.ExtractionStatsFromContext(ctx)

	pgControlRe := regexp.MustCompile(`^.*?pg_control\.tar(\..+$|$)`)
	backupLabelRe := regexp.MustCompile(`^.*?backup_label\.tar(\..+$|$)`)
	for _, tarObject := range tarObjects {
		tarName := tarObject.GetName()
		tarToExtract := internal.NewStorageReaderMaker(tarPartitionFolder, tarName).
			WithStorageSize(tarObject.GetSize())

		// Separate the pg_control tarName from the others to
		// extract it at the end, as to prevent server startup
		// with incomplete backup restoration.  But only if it
		// exists: it won't in the case of WAL-E backup
		// backwards compatibility.
		if pgControlRe.MatchString(tarName) {
			sequentialTarsToExtract = append(sequentialTarsToExtract, tarToExtract)
			stats.AddTarball(tarObject.GetSize(), false)
			continue
		}

		// wal-g creates fictional `backup_label.tar` at the end of backup.
		// It contains values from `pg_stop_backup`. Postgres datadir may have other `backup_label` file
		// from some exclusive backup (likely not ours).
		// We should override it in order to reach correct end of backup point.
		// so, we should extract our `backup_label` after extracting regular tars.
		if backupLabelRe.MatchString(tarName) {
			sequentialTarsToExtract = append(sequentialTarsToExtract, tarToExtract)
			stats.AddTarball(tarObject.GetSize(), false)
			continue
		}

		if skipRedundantTars && !shouldUnwrapTar(tarName, filesMeta, filesToUnwrap) {
			stats.AddTarball(tarObject.GetSize(), true)
			continue
		}

		concurrentTarsToExtract = append(concurrentTarsToExtract, tarToExtract)
		stats.AddTarball(tarObject.GetSize(), false)
	}
	return concurrentTarsToExtract, sequentialTarsToExtract, nil
}

func tarObjectNames(tarObjects []storage.Object) []string {
	names := make([]string, len(tarObjects))
	for i, tarObject := range tarObjects {
		names[i] = tarObject.GetName()
	}
	return names
}
