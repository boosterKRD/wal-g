package internal

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	conf "github.com/wal-g/wal-g/internal/config"
)

// TarBallQueue is used to process multiple tarballs concurrently
type TarBallQueue struct {
	tarsToFillQueue  chan TarBall
	uploadQueue      chan TarBall
	parallelTarballs int
	maxUploadQueue   int
	mutex            sync.Mutex
	started          atomic.Bool

	TarSizeThreshold int64
	// DedicatedFileSize is the size from which a file is packed into a tarball of its own instead
	// of sharing one. Delta restore can only skip or fetch a whole tarball, so keeping large files
	// on their own is what makes skipping worthwhile. Zero disables the behaviour.
	DedicatedFileSize  int64
	AllTarballsSize    atomic.Int64
	TarBallMaker       TarBallMaker
	LastCreatedTarball TarBall
}

func NewTarBallQueue(tarSizeThreshold int64, tarBallMaker TarBallMaker) *TarBallQueue {
	return &TarBallQueue{
		TarSizeThreshold:  tarSizeThreshold,
		DedicatedFileSize: dedicatedFileSize(tarSizeThreshold),
		TarBallMaker:      tarBallMaker,
		started:           atomic.Bool{},
	}
}

// dedicatedFileSize resolves WALG_TAR_DEDICATED_FILE_SIZE, defaulting to half of the tarball size
// threshold when it is not set.
func dedicatedFileSize(tarSizeThreshold int64) int64 {
	if configured := viper.GetInt64(conf.TarDedicatedFileSizeSetting); configured > 0 {
		return configured
	}
	return tarSizeThreshold / 2
}

// IsDedicatedFile tells whether a file of the given size should get a tarball of its own.
func (tarQueue *TarBallQueue) IsDedicatedFile(size int64) bool {
	return tarQueue.DedicatedFileSize > 0 && size >= tarQueue.DedicatedFileSize
}

func (tarQueue *TarBallQueue) StartQueue() error {
	if tarQueue.started.Load() {
		panic("Trying to start already started Queue")
	}
	var err error
	tarQueue.parallelTarballs, err = conf.GetMaxUploadDiskConcurrency()
	if err != nil {
		return err
	}
	tarQueue.maxUploadQueue, err = conf.GetMaxUploadQueue()
	if err != nil {
		return err
	}

	tarQueue.tarsToFillQueue = make(chan TarBall, tarQueue.parallelTarballs)
	tarQueue.uploadQueue = make(chan TarBall, tarQueue.parallelTarballs+tarQueue.maxUploadQueue)
	for i := 0; i < tarQueue.parallelTarballs; i++ {
		tarQueue.NewTarBall(true)
		tarQueue.tarsToFillQueue <- tarQueue.LastCreatedTarball
	}

	tarQueue.started.Store(true)
	return nil
}

// Deque returns a TarBall from the queue. If the context finishes before it
// can do so, it returns the result of ctx.Err().
func (tarQueue *TarBallQueue) Deque(ctx context.Context) (TarBall, error) {
	if !tarQueue.started.Load() {
		panic("Trying to deque from not started Queue")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case tarball := <-tarQueue.tarsToFillQueue:
		return tarball, nil
	}
}

func (tarQueue *TarBallQueue) FinishQueue() error {
	if !tarQueue.started.Load() {
		panic("Trying to stop not started Queue")
	}
	tarQueue.started.Store(false)

	// We have to deque exactly this count of workers
	for i := 0; i < tarQueue.parallelTarballs; i++ {
		tarBall := <-tarQueue.tarsToFillQueue
		if tarBall.TarWriter() == nil {
			// This had written nothing
			continue
		}
		err := tarQueue.CloseTarball(tarBall)
		if err != nil {
			return errors.Wrap(err, "HandleWalkedFSObject: failed to close tarball")
		}
		if err := tarBall.AwaitUploads(); err != nil {
			return err
		}
	}

	// At this point no new tarballs should be put into uploadQueue
	for len(tarQueue.uploadQueue) > 0 {
		select {
		case otb := <-tarQueue.uploadQueue:
			if err := otb.AwaitUploads(); err != nil {
				return err
			}
		default:
		}
	}

	return nil
}

func (tarQueue *TarBallQueue) EnqueueBack(tarBall TarBall) {
	tarQueue.tarsToFillQueue <- tarBall
}

func (tarQueue *TarBallQueue) FinishTarBall(tarBall TarBall) error {
	tarQueue.mutex.Lock()
	defer tarQueue.mutex.Unlock()

	err := tarQueue.CloseTarball(tarBall)
	if err != nil {
		return errors.Wrap(err, "HandleWalkedFSObject: failed to close tarball")
	}

	tarQueue.uploadQueue <- tarBall
	for len(tarQueue.uploadQueue) > tarQueue.maxUploadQueue {
		select {
		case otb := <-tarQueue.uploadQueue:
			if err := otb.AwaitUploads(); err != nil {
				return err
			}
		default:
		}
	}

	tarQueue.NewTarBall(true)
	tarQueue.tarsToFillQueue <- tarQueue.LastCreatedTarball
	return nil
}

// NewDedicatedTarBall creates a tarball that is not part of the fill queue, meant to hold a single
// large file. The caller must pass it to FinishDedicatedTarBall once the file has been packed.
// Callers should still hold a tarball dequeued from the fill queue while doing so, otherwise the
// number of files packed at once is no longer bounded.
func (tarQueue *TarBallQueue) NewDedicatedTarBall() TarBall {
	tarQueue.mutex.Lock()
	defer tarQueue.mutex.Unlock()

	return tarQueue.TarBallMaker.Make(true)
}

// FinishDedicatedTarBall closes and uploads a tarball made by NewDedicatedTarBall. Unlike
// FinishTarBall it does not put a replacement into the fill queue, because this tarball was never
// in it.
func (tarQueue *TarBallQueue) FinishDedicatedTarBall(tarBall TarBall) error {
	tarQueue.mutex.Lock()
	defer tarQueue.mutex.Unlock()

	err := tarQueue.CloseTarball(tarBall)
	if err != nil {
		return errors.Wrap(err, "FinishDedicatedTarBall: failed to close tarball")
	}

	tarQueue.uploadQueue <- tarBall
	for len(tarQueue.uploadQueue) > tarQueue.maxUploadQueue {
		select {
		case otb := <-tarQueue.uploadQueue:
			if err := otb.AwaitUploads(); err != nil {
				return err
			}
		default:
		}
	}

	return nil
}

func (tarQueue *TarBallQueue) CheckSizeAndEnqueueBack(tarBall TarBall) error {
	if tarBall.Size() > tarQueue.TarSizeThreshold {
		return tarQueue.FinishTarBall(tarBall)
	}

	tarQueue.tarsToFillQueue <- tarBall
	return nil
}

// NewTarBall starts writing new tarball
func (tarQueue *TarBallQueue) NewTarBall(dedicatedUploader bool) TarBall {
	tarQueue.LastCreatedTarball = tarQueue.TarBallMaker.Make(dedicatedUploader)
	return tarQueue.LastCreatedTarball
}

func (tarQueue *TarBallQueue) CloseTarball(tarBall TarBall) error {
	tarQueue.AllTarballsSize.Add(tarBall.Size())
	return tarBall.CloseTar()
}
