package internal

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSelectiveInterpreter records what it was handed and reports a fixed set of wanted entries.
type stubSelectiveInterpreter struct {
	wanted map[string]bool
	count  int
	seen   []string
}

func (s *stubSelectiveInterpreter) Interpret(_ io.Reader, header *tar.Header) error {
	s.seen = append(s.seen, header.Name)
	return nil
}

func (s *stubSelectiveInterpreter) InterestingEntryCount(string) (int, bool) {
	return s.count, true
}

func (s *stubSelectiveInterpreter) IsInterestingEntry(name string) bool {
	return s.wanted[name]
}

func buildTar(t *testing.T, names ...string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, name := range names {
		content := bytes.Repeat([]byte("x"), 4096)
		require.NoError(t, writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}))
		_, err := writer.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func TestExtractOneTarUntilDone_StopsAfterTheLastWantedEntry(t *testing.T) {
	tarBytes := buildTar(t, "first", "second", "third")
	source := bytes.NewReader(tarBytes)

	interpreter := &stubSelectiveInterpreter{wanted: map[string]bool{"first": true}, count: 1}

	stoppedEarly, err := extractOneTarUntilDone(interpreter, source, "part_001.tar")

	require.NoError(t, err)
	assert.True(t, stoppedEarly)
	assert.Equal(t, []string{"first"}, interpreter.seen, "entries after the last wanted one are not read")
	assert.NotZero(t, source.Len(), "the rest of the tarball is left unread")
}

// Reaching the count on the very last entry also stops the read: what is left is only the tar
// padding, and reading it is exactly what stopping early is meant to avoid.
func TestExtractOneTarUntilDone_StopsOnTheLastEntryToo(t *testing.T) {
	tarBytes := buildTar(t, "first", "second", "third")
	source := bytes.NewReader(tarBytes)

	interpreter := &stubSelectiveInterpreter{
		wanted: map[string]bool{"first": true, "second": true, "third": true},
		count:  3,
	}

	stoppedEarly, err := extractOneTarUntilDone(interpreter, source, "part_001.tar")

	require.NoError(t, err)
	assert.True(t, stoppedEarly)
	assert.Equal(t, []string{"first", "second", "third"}, interpreter.seen)
}

// An interpreter that cannot count its entries must have the whole tarball read to it, as before.
func TestExtractOneTarUntilDone_PlainInterpreterReadsEverything(t *testing.T) {
	tarBytes := buildTar(t, "first", "second", "third")

	var seen []string
	interpreter := tarInterpreterFunc(func(_ io.Reader, header *tar.Header) error {
		seen = append(seen, header.Name)
		return nil
	})

	stoppedEarly, err := extractOneTarUntilDone(interpreter, bytes.NewReader(tarBytes), "part_001.tar")

	require.NoError(t, err)
	assert.False(t, stoppedEarly)
	assert.Equal(t, []string{"first", "second", "third"}, seen)
}

type tarInterpreterFunc func(reader io.Reader, header *tar.Header) error

func (f tarInterpreterFunc) Interpret(reader io.Reader, header *tar.Header) error {
	return f(reader, header)
}
