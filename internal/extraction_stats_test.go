package internal

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractionStats_NilIsANoop(t *testing.T) {
	var stats *ExtractionStats
	// Nothing to assert beyond not panicking: a nil receiver is the normal state when nobody asked.
	stats.AddTarball(10, true)
	stats.AddRead(10, 5, true)
	stats.AddWritten(10)
}

func TestExtractionStats_Counts(t *testing.T) {
	stats := &ExtractionStats{}

	stats.AddTarball(100, false)
	stats.AddTarball(200, true)
	stats.AddTarball(300, true)
	stats.AddRead(100, 100, false)
	stats.AddWritten(80)
	stats.AddWritten(15)

	assert.EqualValues(t, 3, stats.TarballsTotal.Load())
	assert.EqualValues(t, 600, stats.BytesTotal.Load())
	assert.EqualValues(t, 2, stats.TarballsSkipped.Load())
	assert.EqualValues(t, 500, stats.BytesSkipped.Load())
	assert.EqualValues(t, 1, stats.TarballsRead.Load())
	assert.EqualValues(t, 100, stats.BytesDownloaded.Load())
	assert.EqualValues(t, 0, stats.TarballsCutShort.Load())
	assert.EqualValues(t, 0, stats.BytesNotRead.Load())
	assert.EqualValues(t, 2, stats.FilesWritten.Load())
	assert.EqualValues(t, 95, stats.BytesWritten.Load())
}

// A tarball of 1000 bytes read up to byte 100 and then dropped leaves 900 unread. That is the
// number the summary is for.
func TestExtractionStats_CutShort(t *testing.T) {
	stats := &ExtractionStats{}

	stats.AddRead(1000, 100, true)
	stats.AddRead(1000, 1000, true)
	// Without a known size there is nothing to subtract from.
	stats.AddRead(0, 40, true)

	assert.EqualValues(t, 3, stats.TarballsRead.Load())
	assert.EqualValues(t, 3, stats.TarballsCutShort.Load())
	assert.EqualValues(t, 1140, stats.BytesDownloaded.Load())
	assert.EqualValues(t, 900, stats.BytesNotRead.Load())
}

func TestExtractionStats_Context(t *testing.T) {
	assert.Nil(t, ExtractionStatsFromContext(context.Background()))

	stats := &ExtractionStats{}
	ctx := ContextWithExtractionStats(context.Background(), stats)
	assert.Same(t, stats, ExtractionStatsFromContext(ctx))
}

func TestCountingReadCloser(t *testing.T) {
	reader := &countingReadCloser{ReadCloser: io.NopCloser(strings.NewReader("twelve bytes"))}

	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "twelve bytes", string(content))
	assert.EqualValues(t, 12, reader.count.Load())
}

func TestFormatBytes(t *testing.T) {
	assert.Equal(t, "0 B", FormatBytes(0))
	assert.Equal(t, "1023 B", FormatBytes(1023))
	assert.Equal(t, "1.0 KiB", FormatBytes(1024))
	assert.Equal(t, "13.3 MiB", FormatBytes(13944701))
	assert.Equal(t, "1.9 GiB", FormatBytes(2040109466))
}
