package postgres

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServe_ReturnsOnContextCancel(t *testing.T) {
	// Not t.TempDir(): it embeds the test name in the path, which is long enough to push the
	// socket past the sun_path limit (104 bytes on darwin) and make bind fail with EINVAL.
	tempDir, err := os.MkdirTemp("", "walg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	socketPath := filepath.Join(tempDir, "walg.sock")
	l, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(ctx, l, nil)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancel")
	}
}
