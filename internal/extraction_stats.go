package internal

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
)

// ExtractionStats counts what an extraction did with the tarballs of a backup: how many there
// were, how many it never downloaded, how many it stopped reading before the end, and how many
// bytes went each way. A delta restore prints it at the end, so that the operator can see what the
// delta actually saved.
//
// It travels in the context, and a nil pointer is the normal state: nothing is counted unless
// somebody put one there, and the counters are atomic so the concurrent extraction can bump them
// without coordination. The cost of counting is an increment per tarball and per Read, nothing
// that could be measured next to decompression.
type ExtractionStats struct {
	TarballsTotal    atomic.Int64
	TarballsSkipped  atomic.Int64
	TarballsRead     atomic.Int64
	TarballsCutShort atomic.Int64

	// Sizes as they are in storage, compressed.
	BytesTotal      atomic.Int64
	BytesSkipped    atomic.Int64
	BytesDownloaded atomic.Int64
	// What the cut-short tarballs still held past the point where reading stopped.
	BytesNotRead atomic.Int64

	// Regular files written to disk, uncompressed. For an incremented file this is the size of
	// the increment rather than of the whole file.
	FilesWritten atomic.Int64
	BytesWritten atomic.Int64
}

type extractionStatsKey struct{}

// ContextWithExtractionStats returns a context carrying stats for everything extracted under it.
func ContextWithExtractionStats(ctx context.Context, stats *ExtractionStats) context.Context {
	return context.WithValue(ctx, extractionStatsKey{}, stats)
}

// ExtractionStatsFromContext returns the stats to count into, or nil when nobody is counting.
func ExtractionStatsFromContext(ctx context.Context) *ExtractionStats {
	stats, _ := ctx.Value(extractionStatsKey{}).(*ExtractionStats)
	return stats
}

// AddTarball records a tarball of the backup and whether the extraction is going to read it at
// all. Safe to call on a nil receiver.
func (stats *ExtractionStats) AddTarball(size int64, skipped bool) {
	if stats == nil {
		return
	}
	stats.TarballsTotal.Add(1)
	stats.BytesTotal.Add(size)
	if skipped {
		stats.TarballsSkipped.Add(1)
		stats.BytesSkipped.Add(size)
	}
}

// AddRead records one tarball that has been read: how much of it was downloaded, how large it is
// in storage, and whether reading stopped before the end. Safe to call on a nil receiver.
func (stats *ExtractionStats) AddRead(size, downloaded int64, cutShort bool) {
	if stats == nil {
		return
	}
	stats.TarballsRead.Add(1)
	stats.BytesDownloaded.Add(downloaded)
	if cutShort {
		stats.TarballsCutShort.Add(1)
		if size > downloaded {
			stats.BytesNotRead.Add(size - downloaded)
		}
	}
}

// AddWritten records one regular file written to disk. Safe to call on a nil receiver.
func (stats *ExtractionStats) AddWritten(size int64) {
	if stats == nil {
		return
	}
	stats.FilesWritten.Add(1)
	stats.BytesWritten.Add(size)
}

// countingReadCloser counts the bytes read through it. Reading is a plain pass-through, the count
// is an atomic add per call.
type countingReadCloser struct {
	io.ReadCloser
	count atomic.Int64
}

func (reader *countingReadCloser) Read(p []byte) (int, error) {
	n, err := reader.ReadCloser.Read(p)
	reader.count.Add(int64(n))
	return n, err
}

// FormatBytes renders a byte count the way the summary prints it: whole bytes below a kilobyte,
// one decimal above, in binary units.
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f EiB", value/unit)
}
