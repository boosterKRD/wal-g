package postgres

import (
	"archive/tar"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/utility"
)

type IncrementalTarInterpreter interface {
	internal.TarInterpreter
	GetUnwrapResult() *UnwrapResult
}

// FileTarInterpreter extracts input to disk.
type FileTarInterpreter struct {
	DBDataDirectory string
	Sentinel        BackupSentinelDto
	FilesMetadata   FilesMetadataDto
	FilesToUnwrap   map[string]bool
	UnwrapResult    *UnwrapResult

	createNewIncrementalFiles bool
	// earlyStop lets extraction stop reading a tarball once every entry it cares about has been
	// handled. It is opt-in because it relies on TarFileSets listing every entry of a tarball,
	// which only holds for tarballs written by the PostgreSQL composers.
	earlyStop bool
	// stats counts the files written to disk, when somebody is collecting. Nil otherwise.
	stats *internal.ExtractionStats
}

func NewFileTarInterpreter(
	dbDataDirectory string, sentinel BackupSentinelDto, filesMetadata FilesMetadataDto,
	filesToUnwrap map[string]bool, createNewIncrementalFiles bool,
) *FileTarInterpreter {
	return &FileTarInterpreter{
		DBDataDirectory:           dbDataDirectory,
		Sentinel:                  sentinel,
		FilesMetadata:             filesMetadata,
		FilesToUnwrap:             filesToUnwrap,
		UnwrapResult:              newUnwrapResult(),
		createNewIncrementalFiles: createNewIncrementalFiles,
	}
}

// EnableEarlyStop makes extraction stop reading a tarball as soon as everything needed from it has
// been written, instead of reading the remaining entries for nothing. On object storage this cuts
// the download short.
func (tarInterpreter *FileTarInterpreter) EnableEarlyStop() {
	tarInterpreter.earlyStop = true
}

// SetExtractionStats makes the interpreter count what it writes into stats.
func (tarInterpreter *FileTarInterpreter) SetExtractionStats(stats *internal.ExtractionStats) {
	tarInterpreter.stats = stats
}

// InterestingEntryCount reports how many entries of the named tarball are going to be acted upon.
func (tarInterpreter *FileTarInterpreter) InterestingEntryCount(tarName string) (int, bool) {
	if !tarInterpreter.earlyStop || tarInterpreter.FilesToUnwrap == nil {
		return 0, false
	}
	// Without the tar file sets there is no way to know what a tarball holds, so it has to be read
	// to the end. This is the case for backups taken with WALG_WITHOUT_FILES_METADATA.
	entryNames, ok := tarInterpreter.FilesMetadata.TarFileSets[tarName]
	if !ok || len(entryNames) == 0 {
		return 0, false
	}

	count := 0
	for _, entryName := range entryNames {
		if tarInterpreter.IsInterestingEntry(entryName) {
			count++
		}
	}
	return count, true
}

// IsInterestingEntry tells whether an entry is going to be acted upon during extraction. Files that
// are not being unwrapped are skipped, but anything that is not a regular file of the backup, a
// directory or a link for instance, is always created and therefore always counts.
func (tarInterpreter *FileTarInterpreter) IsInterestingEntry(name string) bool {
	if tarInterpreter.FilesToUnwrap == nil || tarInterpreter.FilesToUnwrap[name] {
		return true
	}
	_, isBackupFile := tarInterpreter.FilesMetadata.Files[name]
	return !isBackupFile
}

func (tarInterpreter *FileTarInterpreter) GetUnwrapResult() *UnwrapResult {
	return tarInterpreter.UnwrapResult
}

// TODO : unit tests
func (tarInterpreter *FileTarInterpreter) unwrapRegularFileOld(fileReader io.Reader,
	fileInfo *tar.Header,
	targetPath string,
	fsync bool) error {
	if tarInterpreter.FilesToUnwrap != nil {
		if _, ok := tarInterpreter.FilesToUnwrap[fileInfo.Name]; !ok {
			// don't have to unwrap it this time
			tracelog.DebugLogger.Printf("Don't have to unwrap '%s' this time\n", fileInfo.Name)
			return nil
		}
	}
	fileDescription, haveFileDescription := tarInterpreter.FilesMetadata.Files[fileInfo.Name]

	// If this file is incremental we use it's base version from incremental path
	if haveFileDescription && tarInterpreter.Sentinel.IsIncremental() && fileDescription.IsIncremented {
		err := ApplyFileIncrement(targetPath, fileReader, tarInterpreter.createNewIncrementalFiles, fsync)
		if err == nil {
			tarInterpreter.stats.AddWritten(fileInfo.Size)
		}
		return errors.Wrapf(err, "Interpret: failed to apply increment for '%s'", targetPath)
	}
	err := PrepareDirs(fileInfo.Name, targetPath)
	if err != nil {
		return errors.Wrap(err, "Interpret: failed to create all directories")
	}
	file, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return errors.Wrapf(err, "failed to create new file: '%s'", targetPath)
	}
	defer utility.LoggedClose(file, "")

	// Everything that reaches this point is written from start to end, so the whole file passes by
	// and can be checked against the checksum the backup recorded for it. The incremented files,
	// which carry changed pages rather than a whole file, returned above.
	verifier := tarInterpreter.newChecksumVerifier(fileInfo.Name, true)
	if err := utility.WriteLocalFile(verifier.wrap(fileReader), fileInfo, file, fsync); err != nil {
		return err
	}
	if err := verifier.verify(); err != nil {
		return err
	}
	tarInterpreter.stats.AddWritten(fileInfo.Size)
	return nil
}

// Interpret extracts a tar file to disk and creates needed directories.
// Returns the first error encountered. Calls fsync after each file
// is written successfully.
func (tarInterpreter *FileTarInterpreter) Interpret(fileReader io.Reader, fileInfo *tar.Header) error {
	tracelog.DebugLogger.Println("Interpreting: ", fileInfo.Name)
	targetPath := path.Join(tarInterpreter.DBDataDirectory, fileInfo.Name)
	targetPath = filepath.ToSlash(targetPath)
	fsync := !viper.GetBool(conf.TarDisableFsyncSetting)
	switch fileInfo.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		// temporary switch to determine if new unwrap logic should be used
		if useNewUnwrapImplementation {
			return tarInterpreter.unwrapRegularFileNew(fileReader, fileInfo, targetPath, fsync)
		}
		return tarInterpreter.unwrapRegularFileOld(fileReader, fileInfo, targetPath, fsync)
	case tar.TypeDir:
		err := os.MkdirAll(targetPath, 0750)
		if err != nil {
			return errors.Wrapf(err, "Interpret: failed to create all directories in %s", targetPath)
		}
		if err = os.Chmod(targetPath, os.FileMode(fileInfo.Mode)); err != nil {
			return errors.Wrap(err, "Interpret: chmod failed")
		}
	case tar.TypeLink:
		if err := os.Link(fileInfo.Name, targetPath); err != nil {
			return errors.Wrapf(err, "Interpret: failed to create hardlink %s", targetPath)
		}
	case tar.TypeSymlink:
		if err := os.Symlink(fileInfo.Name, targetPath); err != nil {
			return errors.Wrapf(err, "Interpret: failed to create symlink %s", targetPath)
		}
	}
	return nil
}

// PrepareDirs makes sure all dirs exist
func PrepareDirs(fileName string, targetPath string) error {
	if fileName == targetPath {
		return nil // because it runs in the local directory
	}
	base := filepath.Base(fileName)
	dir := strings.TrimSuffix(targetPath, base)
	err := os.MkdirAll(dir, 0750)
	return err
}
