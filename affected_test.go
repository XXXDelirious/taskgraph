package task_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	task "github.com/XXXDelirious/taskgraph"
)

// newAffectedRepo copies testdata/affected into a new git repository, adds
// some source files and commits everything on main.
func newAffectedRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/affected")))
	for _, f := range []string{"apps/web/src/main.go", "apps/api/src/main.go", "apps/api/src/old.go", "libs/shared/lib.go", "libs/shared/lib_test.go"} {
		writeFile(t, dir, f, "package x\n")
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, ".gitignore", ".task/\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func affectedByName(t *testing.T, dir, since string) map[string]*task.AffectedTask {
	t.Helper()
	e := newExplainExecutor(t, dir)
	changed, err := e.ChangedFiles(t.Context(), since)
	require.NoError(t, err)
	affected, err := e.AffectedTasks(changed)
	require.NoError(t, err)
	m := map[string]*task.AffectedTask{}
	for _, a := range affected {
		m[a.Task] = a
	}
	return m
}

func TestAffectedNothingChanged(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	assert.Empty(t, affectedByName(t, dir, ""))
}

func TestAffectedThroughDeps(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "apps/web/src/main.go", "package web // changed\n")
	git(t, dir, "commit", "-q", "-am", "web")

	affected := affectedByName(t, dir, "main")
	assert.Len(t, affected, 3)
	assert.Equal(t, []string{"apps/web/src/main.go"}, affected["web:build"].Files)
	assert.Equal(t, []string{"web:build"}, affected["web:test"].Via)
	assert.Equal(t, []string{"web:test"}, affected["test"].Via)
	assert.NotContains(t, affected, "api:test")
	assert.NotContains(t, affected, "lint", "tasks without sources are not affected by file changes")

	assert.Empty(t, affectedByName(t, dir, ""), "the change is committed, so it only counts with --since")
}

func TestAffectedSharedLibraryAndExcludes(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	writeFile(t, dir, "libs/shared/lib_test.go", "package x // test-only change\n")
	affected := affectedByName(t, dir, "")
	assert.NotContains(t, affected, "shared:build", "excluded sources do not count")
	assert.Contains(t, affected, "helper", "helper's sources have no exclude")

	writeFile(t, dir, "libs/shared/lib.go", "package x // changed\n")
	affected = affectedByName(t, dir, "")
	for _, name := range []string{"shared:build", "web:build", "api:build", "web:test", "api:test", "test", "helper"} {
		assert.Contains(t, affected, name)
	}
	assert.Equal(t, []string{"libs/shared/lib.go"}, affected["shared:build"].Files)
	assert.Equal(t, []string{"shared:build"}, affected["api:build"].Via)
}

func TestAffectedDeletedFileAndTaskfile(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "apps/api/src/old.go")))
	affected := affectedByName(t, dir, "")
	assert.Equal(t, []string{"apps/api/src/old.go"}, affected["api:build"].Files)
	assert.NotContains(t, affected, "web:build")

	dir = newAffectedRepo(t)
	data, err := os.ReadFile(filepath.Join(dir, "apps/web/Taskfile.yml"))
	require.NoError(t, err)
	writeFile(t, dir, "apps/web/Taskfile.yml", string(data)+"# edited\n")
	affected = affectedByName(t, dir, "")
	assert.True(t, affected["web:build"].TaskfileChanged)
	assert.True(t, affected["web:test"].TaskfileChanged)
	assert.NotContains(t, affected, "api:build")
}

func TestAffectedIgnoresTaskStateDir(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	// Without the .gitignore entry, .task/ shows up as untracked.
	require.NoError(t, os.Remove(filepath.Join(dir, ".gitignore")))
	git(t, dir, "commit", "-q", "-am", "no ignore")
	require.NoError(t, newExplainExecutor(t, dir).Run(t.Context(), &task.Call{Task: "web:build"}))

	e := newExplainExecutor(t, dir)
	changed, err := e.ChangedFiles(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestAffectedCallAndOutput(t *testing.T) {
	t.Parallel()

	dir := newAffectedRepo(t)
	writeFile(t, dir, "apps/api/src/main.go", "package api // changed\n")
	writeFile(t, dir, "libs/shared/helper.go", "package x\n")

	e := newExplainExecutor(t, dir)
	changed, err := e.ChangedFiles(t.Context(), "")
	require.NoError(t, err)

	a, err := e.AffectedCall(changed, &task.Call{Task: "api:test"})
	require.NoError(t, err)
	require.NotNil(t, a)
	a, err = e.AffectedCall(changed, &task.Call{Task: "lint"})
	require.NoError(t, err)
	assert.Nil(t, a)

	affected, err := e.AffectedTasks(changed)
	require.NoError(t, err)

	var text bytes.Buffer
	require.NoError(t, e.WriteAffected(&text, affected, false))
	assert.Equal(t, `test          via web:test, api:test
web:test      via web:build
web:build     via shared:build
shared:build  libs/shared/helper.go
api:test      via api:build
api:build     apps/api/src/main.go; via shared:build
`, text.String(), "tasks come in graph order, and internal tasks are left out")

	var out bytes.Buffer
	require.NoError(t, e.WriteAffected(&out, affected, true))
	var decoded []task.AffectedTask
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Len(t, decoded, 6)
}
