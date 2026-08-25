package internal

import (
	"slices"
	"time"
)

const MaxCorruptBlocksInFileDesc int = 10

type BackupFileDescription struct {
	IsIncremented bool // should never be both incremented and Skipped
	IsSkipped     bool
	MTime         time.Time
	CorruptBlocks *CorruptBlocksInfo `json:",omitempty"`
	UpdatesCount  uint64
	// Size of the file on disk at backup time. For incremented files this is the size of the
	// whole file, not the size of the increment stored in the tarball.
	Size int64 `json:",omitempty"`
	// Checksum of the whole file as it was on disk at backup time, hex encoded. Empty when the
	// file was not read in full, so it is absent for skipped files and for incremented files
	// backed up via WAL delta bitmaps.
	Checksum string `json:",omitempty"`
	// Algorithm used to compute Checksum. Stored explicitly so it can be changed later without
	// breaking backups made by older versions.
	ChecksumAlgo string `json:",omitempty"`
}

func NewBackupFileDescription(isIncremented, isSkipped bool, modTime time.Time) *BackupFileDescription {
	return &BackupFileDescription{IsIncremented: isIncremented, IsSkipped: isSkipped, MTime: modTime}
}

type CorruptBlocksInfo struct {
	CorruptBlocksCount int
	SomeCorruptBlocks  []uint32
}

func (desc *BackupFileDescription) SetCorruptBlocks(corruptBlockNumbers []uint32, storeAllBlocks bool) {
	if len(corruptBlockNumbers) == 0 {
		return
	}
	slices.Sort(corruptBlockNumbers)

	corruptBlocksCount := len(corruptBlockNumbers)
	// write no more than MaxCorruptBlocksInFileDesc
	someCorruptBlocks := make([]uint32, 0)
	for idx, blockNo := range corruptBlockNumbers {
		if !storeAllBlocks && idx >= MaxCorruptBlocksInFileDesc {
			break
		}
		someCorruptBlocks = append(someCorruptBlocks, blockNo)
	}
	desc.CorruptBlocks = &CorruptBlocksInfo{
		CorruptBlocksCount: corruptBlocksCount,
		SomeCorruptBlocks:  someCorruptBlocks,
	}
}

type BackupFileList map[string]BackupFileDescription
