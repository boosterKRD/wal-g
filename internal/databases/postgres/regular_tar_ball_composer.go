package postgres

import (
	"archive/tar"
	"context"
	"os"

	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/internal/crypto"
	"golang.org/x/sync/errgroup"
)

type RegularTarBallComposer struct {
	tarBallQueue  *internal.TarBallQueue
	tarFilePacker *TarBallFilePackerImpl
	crypter       crypto.Crypter
	files         internal.BundleFiles
	tarFileSets   internal.TarFileSets
	errorGroup    *errgroup.Group
	ctx           context.Context //nolint:containedctx // errgroup root feeds async AddFile during filepath.Walk
	reqCtx        context.Context //nolint:containedctx // request ctx; outlives errorGroup ctx for background part uploads
}

func NewRegularTarBallComposer(
	ctx context.Context,
	tarBallQueue *internal.TarBallQueue,
	tarBallFilePacker *TarBallFilePackerImpl,
	files internal.BundleFiles,
	tarFileSets internal.TarFileSets,
	crypter crypto.Crypter,
) *RegularTarBallComposer {
	errorGroup, egCtx := errgroup.WithContext(ctx)
	return &RegularTarBallComposer{
		tarBallQueue:  tarBallQueue,
		tarFilePacker: tarBallFilePacker,
		crypter:       crypter,
		files:         files,
		tarFileSets:   tarFileSets,
		errorGroup:    errorGroup,
		ctx:           egCtx,
		reqCtx:        ctx,
	}
}

type RegularTarBallComposerMaker struct {
	filePackerOptions TarBallFilePackerOptions
	files             internal.BundleFiles
	tarFileSets       internal.TarFileSets
}

func NewRegularTarBallComposerMaker(
	filePackerOptions TarBallFilePackerOptions, files internal.BundleFiles, tarFileSets internal.TarFileSets,
) *RegularTarBallComposerMaker {
	return &RegularTarBallComposerMaker{
		filePackerOptions: filePackerOptions,
		files:             files,
		tarFileSets:       tarFileSets,
	}
}

func (maker *RegularTarBallComposerMaker) Make(ctx context.Context, bundle *Bundle) (internal.TarBallComposer, error) {
	bundleFiles := maker.files
	tarFileSets := maker.tarFileSets
	tarBallFilePacker := NewTarBallFilePacker(bundle.DeltaMap,
		bundle.IncrementFromLsn, bundleFiles, maker.filePackerOptions)
	tarBallFilePacker.EnableChecksums()
	if bundle.IncrementFromChkpNum != nil {
		tarBallFilePacker.IncrementFromChkpNum = bundle.IncrementFromChkpNum
	}
	return NewRegularTarBallComposer(ctx, bundle.TarBallQueue, tarBallFilePacker, bundleFiles, tarFileSets, bundle.Crypter), nil
}

func (c *RegularTarBallComposer) AddFile(info *internal.ComposeFileInfo) {
	// A large file goes into a tarball of its own, so that delta restore can skip or fetch it
	// without dragging unrelated files along. Incremented files are left out of this: only their
	// changed pages are stored, so a tarball of their own would usually be a tiny object.
	if !info.IsIncremented && c.tarBallQueue.IsDedicatedFile(info.Header.Size) {
		c.addFileToDedicatedTarBall(info)
		return
	}

	tarBall, err := c.tarBallQueue.Deque(c.ctx)
	if err != nil {
		return
	}
	tarBall.SetUp(c.reqCtx, c.crypter)
	c.tarFileSets.AddFile(tarBall.Name(), info.Header.Name)
	c.errorGroup.Go(func() error {
		err := c.tarFilePacker.PackFileIntoTar(c.ctx, info, tarBall)
		if err != nil {
			return err
		}
		return c.tarBallQueue.CheckSizeAndEnqueueBack(tarBall)
	})
}

// addFileToDedicatedTarBall packs one file into a tarball that holds nothing else. A tarball is
// still taken out of the fill queue and returned untouched afterwards, so that the number of files
// being packed at once stays bounded by the queue as before.
func (c *RegularTarBallComposer) addFileToDedicatedTarBall(info *internal.ComposeFileInfo) {
	queueSlot, err := c.tarBallQueue.Deque(c.ctx)
	if err != nil {
		return
	}
	tarBall := c.tarBallQueue.NewDedicatedTarBall()
	tarBall.SetUp(c.reqCtx, c.crypter)
	c.tarFileSets.AddFile(tarBall.Name(), info.Header.Name)
	c.errorGroup.Go(func() error {
		defer c.tarBallQueue.EnqueueBack(queueSlot)
		err := c.tarFilePacker.PackFileIntoTar(c.ctx, info, tarBall)
		if err != nil {
			return err
		}
		return c.tarBallQueue.FinishDedicatedTarBall(tarBall)
	})
}

func (c *RegularTarBallComposer) AddHeader(fileInfoHeader *tar.Header, info os.FileInfo) error {
	tarBall, err := c.tarBallQueue.Deque(c.ctx)
	if err != nil {
		return c.errorGroup.Wait()
	}
	tarBall.SetUp(c.reqCtx, c.crypter)
	defer c.tarBallQueue.EnqueueBack(tarBall)
	c.tarFileSets.AddFile(tarBall.Name(), fileInfoHeader.Name)
	c.files.AddFile(fileInfoHeader, info, false)
	return tarBall.TarWriter().WriteHeader(fileInfoHeader)
}

func (c *RegularTarBallComposer) SkipFile(tarHeader *tar.Header, fileInfo os.FileInfo) {
	c.files.AddSkippedFile(tarHeader, fileInfo)
}

func (c *RegularTarBallComposer) FinishComposing() (internal.TarFileSets, error) {
	err := c.errorGroup.Wait()
	if err != nil {
		return nil, err
	}
	return c.tarFileSets, nil
}

func (c *RegularTarBallComposer) GetFiles() internal.BundleFiles {
	return c.files
}
