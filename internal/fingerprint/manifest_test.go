package fingerprint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"

	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// upstreamChecksum is the checksum algorithm from Task before per-file
// manifests were added. checksumFiles must keep producing the same value so
// .task/checksum files written by Task stay valid.
func upstreamChecksum(t *testing.T, files []string) string {
	t.Helper()
	h := xxh3.New()
	for _, name := range files {
		_, err := io.Copy(h, strings.NewReader(filepath.Base(name)))
		require.NoError(t, err)
		f, err := os.Open(name)
		require.NoError(t, err)
		_, err = io.Copy(h, f)
		f.Close()
		require.NoError(t, err)
	}
	sum := h.Sum128()
	return fmt.Sprintf("%x%x", sum.Hi, sum.Lo)
}

func newSourcesTask(t *testing.T) (*ast.Task, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "b.txt"), []byte("beta"), 0o644))
	return &ast.Task{
		Task:    "build",
		Dir:     dir,
		Sources: []*ast.Glob{{Glob: "src/*.txt"}},
	}, dir
}

func TestChecksumFilesMatchesUpstream(t *testing.T) {
	t.Parallel()

	task, dir := newSourcesTask(t)
	hash, files, err := NewChecksumChecker(t.TempDir(), false).checksumFiles(task)
	require.NoError(t, err)

	assert.Equal(t, upstreamChecksum(t, []string{
		filepath.Join(dir, "src", "a.txt"),
		filepath.Join(dir, "src", "b.txt"),
	}), hash)
	assert.Len(t, files, 2)
	assert.Contains(t, files, "src/a.txt")
	assert.Contains(t, files, "src/b.txt")
}

func TestChecksumCheckerWritesManifest(t *testing.T) {
	t.Parallel()

	task, _ := newSourcesTask(t)
	tempDir := t.TempDir()
	checker := NewChecksumChecker(tempDir, false)

	_, err := checker.IsUpToDate(task)
	require.NoError(t, err)
	files, err := readManifest(checker.manifestFilePath(task))
	require.NoError(t, err)
	assert.Len(t, files, 2)

	require.NoError(t, checker.OnError(task))
	assert.False(t, fileExists(checker.manifestFilePath(task)), "OnError should remove the manifest")
}

func TestChecksumCheckerDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	task, _ := newSourcesTask(t)
	tempDir := t.TempDir()
	_, err := NewChecksumChecker(tempDir, true).IsUpToDate(task)
	require.NoError(t, err)

	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestDiffManifests(t *testing.T) {
	t.Parallel()

	changes := diffManifests(
		map[string]string{"kept": "1", "edited": "1", "gone": "1"},
		map[string]string{"kept": "1", "edited": "2", "new": "1"},
	)
	assert.Equal(t, []FileChange{
		{Path: "edited", Change: ChangeModified},
		{Path: "gone", Change: ChangeRemoved},
		{Path: "new", Change: ChangeAdded},
	}, changes)
}
