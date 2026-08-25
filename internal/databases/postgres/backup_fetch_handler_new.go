package postgres

import (
	"context"
	"fmt"

	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/pkg/storages/storage"
	"github.com/wal-g/wal-g/utility"
)

func GetFetcherNew(dbDataDirectory, fileMask, restoreSpecPath string, skipRedundantTars bool,
	extractProv ExtractProvider, options ...FetchOption,
) internal.Fetcher {
	fetchOpts := newFetchOptions(options)
	return func(ctx context.Context, rootFolder storage.Folder, backup internal.Backup) {
		pgBackup := ToPgBackup(backup)
		filesToUnwrap, err := pgBackup.GetFilesToUnwrap(ctx, fileMask)
		tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n", err)

		var spec *TablespaceSpec
		if restoreSpecPath != "" {
			delete(filesToUnwrap, TablespaceMapFilename)
			spec = &TablespaceSpec{}
			err := readRestoreSpec(restoreSpecPath, spec)
			errMessege := fmt.Sprintf("Invalid restore specification path %s\n", restoreSpecPath)
			tracelog.ErrorLogger.FatalfOnError(errMessege, err)
		}

		dataDirectory := utility.ResolveSymlink(dbDataDirectory)
		deltaRestore := false
		if fetchOpts.deltaRestore {
			_, filesMeta, err := pgBackup.GetSentinelAndFilesMetadata(ctx)
			tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n", err)

			filesToUnwrap, deltaRestore, err = prepareDeltaRestore(dataDirectory, filesMeta, filesToUnwrap)
			tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n", err)
		}

		// A delta restore knows which files it is going to overwrite, everything else needs an
		// empty directory before starting a deltaFetch
		if !deltaRestore {
			isEmpty, err := utility.IsDirectoryEmpty(dbDataDirectory, nil)
			tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n", err)

			if !isEmpty {
				tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n",
					NewNonEmptyDBDataDirectoryError(dbDataDirectory))
			}
		}
		config := NewFetchConfig(
			dataDirectory,
			pgBackup,
			rootFolder,
			spec,
			filesToUnwrap,
			skipRedundantTars,
			deltaRestore,
			extractProv,
		)
		err = deltaFetchRecursionNew(ctx, config)
		tracelog.ErrorLogger.FatalfOnError("Failed to fetch backup: %v\n", err)
	}
}

// TODO : unit tests
// deltaFetchRecursion function composes Backup object and recursively searches for necessary base backup
func deltaFetchRecursionNew(ctx context.Context, cfg *FetchConfig) error {
	backup, err := NewBackupInStorage(
		ctx,
		cfg.rootFolder.GetSubFolder(utility.BaseBackupPath),
		cfg.backup.Name,
		cfg.backup.GetStorageName(),
	)
	if err != nil {
		return err
	}
	sentinelDto, filesMetaDto, err := backup.GetSentinelAndFilesMetadata(ctx)
	if err != nil {
		return err
	}
	cfg.tablespaceSpec = chooseTablespaceSpecification(sentinelDto.TablespaceSpec, cfg.tablespaceSpec)
	if sentinelDto.TablespaceSpec == nil {
		sentinelDto.TablespaceSpec = cfg.tablespaceSpec
	} else {
		*sentinelDto.TablespaceSpec = *cfg.tablespaceSpec
	}

	if sentinelDto.IsIncremental() {
		tracelog.InfoLogger.Printf("Delta %v at LSN %s \n",
			cfg.backup.Name,
			*(sentinelDto.BackupStartLSN))
		baseFilesToUnwrap, err := GetBaseFilesToUnwrap(filesMetaDto.Files, cfg.filesToUnwrap)
		if err != nil {
			return err
		}
		unwrapResult, err := backup.unwrapNew(ctx, cfg.dbDataDirectory, cfg.filesToUnwrap,
			false, cfg.skipRedundantTars || cfg.deltaRestore, cfg.extractProv)
		if err != nil {
			return err
		}
		cfg.filesToUnwrap = baseFilesToUnwrap
		cfg.backup.Name = *sentinelDto.IncrementFrom
		if cfg.skipRedundantTars {
			// if we skip redundant tars we should exclude files that
			// no longer need any additional information (completed ones)
			cfg.SkipRedundantFiles(unwrapResult)
		}
		tracelog.InfoLogger.Printf("%v fetched. Downgrading from LSN %s to LSN %s \n",
			cfg.backup.Name,
			*(sentinelDto.BackupStartLSN),
			*(sentinelDto.IncrementFromLSN))
		err = deltaFetchRecursionNew(ctx, cfg)
		if err != nil {
			return err
		}

		return nil
	}

	tracelog.InfoLogger.Printf("%s reached. Applying base backup... \n",
		*(sentinelDto.BackupStartLSN))
	_, err = backup.unwrapNew(ctx, cfg.dbDataDirectory, cfg.filesToUnwrap,
		false, cfg.skipRedundantTars || cfg.deltaRestore, cfg.extractProv)
	return err
}
