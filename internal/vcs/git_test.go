package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRepo creates a git repository with one commit on main.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "test")
	run(t, dir, "config", "commit.gpgsign", "false")
	write(t, dir, "kept.txt", "kept")
	write(t, dir, "edited.txt", "v1")
	write(t, dir, "removed.txt", "bye")
	write(t, dir, "renamed.txt", "moving")
	write(t, dir, ".gitignore", "ignored.txt\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func rel(t *testing.T, dir string, files []string) []string {
	t.Helper()
	root, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(files))
	for _, f := range files {
		r, err := filepath.Rel(root, f)
		require.NoError(t, err)
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

func TestChangedFilesUncommitted(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	write(t, dir, "edited.txt", "v2")
	write(t, dir, "sub/new.txt", "new")
	write(t, dir, "ignored.txt", "ignored")
	require.NoError(t, os.Remove(filepath.Join(dir, "removed.txt")))
	run(t, dir, "mv", "renamed.txt", "moved.txt")

	files, err := ChangedFiles(t.Context(), filepath.Join(dir, "sub"), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"edited.txt", "moved.txt", "removed.txt", "renamed.txt", "sub/new.txt"}, rel(t, dir, files))
}

func TestChangedFilesSince(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	run(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "feature.txt", "committed on the branch")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "feature")

	// A change on main after the branch point is not part of the branch.
	run(t, dir, "checkout", "-q", "main")
	write(t, dir, "main-only.txt", "main")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "main")
	run(t, dir, "checkout", "-q", "feature")

	write(t, dir, "edited.txt", "uncommitted")

	files, err := ChangedFiles(t.Context(), dir, "main")
	require.NoError(t, err)
	assert.Equal(t, []string{"edited.txt", "feature.txt"}, rel(t, dir, files))

	files, err = ChangedFiles(t.Context(), dir, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"edited.txt"}, rel(t, dir, files), "without --since, only uncommitted changes count")

	_, err = ChangedFiles(t.Context(), dir, "no-such-branch")
	assert.ErrorContains(t, err, `cannot compare with "no-such-branch"`)
}

func TestChangedFilesOutsideRepo(t *testing.T) { //nolint:paralleltest // uses t.Setenv
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	// Stop git from finding a repository above the temp dir.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	_, err := ChangedFiles(t.Context(), dir, "")
	assert.Error(t, err)
}
