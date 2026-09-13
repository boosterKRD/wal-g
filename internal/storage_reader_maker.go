package internal

import (
	"context"
	"io"

	"github.com/wal-g/wal-g/pkg/storages/storage"
)

// StorageReaderMaker creates readers for downloading from storage
type StorageReaderMaker struct {
	Folder          storage.Folder
	storagePath     string
	localPath       string
	StorageFileType FileType
	FileMode        int64
	// storageSize is how large the object is in storage, when the caller happened to know it
	// from a listing. Zero means unknown; it only feeds the extraction stats.
	storageSize int64
}

func NewStorageReaderMaker(folder storage.Folder, relativePath string) *StorageReaderMaker {
	return &StorageReaderMaker{
		Folder:          folder,
		storagePath:     relativePath,
		localPath:       relativePath,
		StorageFileType: TarFileType,
	}
}

func NewRegularFileStorageReaderMarker(folder storage.Folder, storagePath, localPath string, fileMode int64) *StorageReaderMaker {
	return &StorageReaderMaker{
		Folder:          folder,
		storagePath:     storagePath,
		localPath:       localPath,
		StorageFileType: RegularFileType,
		FileMode:        fileMode,
	}
}

// WithStorageSize records the object's size from a listing, so that the extraction stats can tell
// how much of a tarball was left unread when reading stopped early.
func (readerMaker *StorageReaderMaker) WithStorageSize(size int64) *StorageReaderMaker {
	readerMaker.storageSize = size
	return readerMaker
}

// StorageSize returns the size recorded by WithStorageSize, zero when unknown.
func (readerMaker *StorageReaderMaker) StorageSize() int64 { return readerMaker.storageSize }

func (readerMaker *StorageReaderMaker) StoragePath() string { return readerMaker.storagePath }

func (readerMaker *StorageReaderMaker) LocalPath() string { return readerMaker.localPath }

func (readerMaker *StorageReaderMaker) Reader(ctx context.Context) (io.ReadCloser, error) {
	return readerMaker.Folder.ReadObject(ctx, readerMaker.storagePath)
}

func (readerMaker *StorageReaderMaker) FileType() FileType { return readerMaker.StorageFileType }

func (readerMaker *StorageReaderMaker) Mode() int64 { return readerMaker.FileMode }
