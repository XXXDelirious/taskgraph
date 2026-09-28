package task_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/internal/fingerprint"
)

// newExplainProject copies testdata/explain into a temp dir, so tests can
// run tasks and change source files without touching the repository.
func newExplainProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/explain/Taskfile.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), data, 0o644)) //nolint:gosec // dir is a test temp dir
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	writeFile(t, dir, "src/a.txt", "a")
	writeFile(t, dir, "src/b.txt", "b")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644)) //nolint:gosec // test helper writing into a temp dir
}

func newExplainExecutor(t *testing.T, dir string, opts ...task.ExecutorOption) *task.Executor {
	t.Helper()
	var buff SyncBuffer
	opts = append([]task.ExecutorOption{
		task.WithDir(dir),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
		task.WithSilent(true),
	}, opts...)
	e := task.NewExecutor(opts...)
	require.NoError(t, e.Setup())
	return e
}

func explainOne(t *testing.T, e *task.Executor, name string) *task.TaskExplanation {
	t.Helper()
	exs, err := e.ExplainTasks(t.Context(), &task.Call{Task: name})
	require.NoError(t, err)
	require.Len(t, exs, 1)
	return exs[0]
}

func TestExplainBeforeAndAfterRun(t *testing.T) {
	t.Parallel()

	dir := newExplainProject(t)

	ex := explainOne(t, newExplainExecutor(t, dir), "build")
	assert.Equal(t, task.VerdictWillRun, ex.Verdict)
	assert.Contains(t, ex.Reasons, "no previous successful run is recorded")
	assert.Equal(t, []string{"out.txt"}, ex.Fingerprint.MissingGenerates)

	require.Len(t, ex.Children, 3, "two deps, plus the task called from cmds")
	gen, check, notify := ex.Children[0], ex.Children[1], ex.Children[2]
	assert.Equal(t, task.VerdictWillRun, gen.Verdict)
	assert.Equal(t, []string{"test -f src/gen.txt"}, gen.Fingerprint.FailedStatus)
	assert.Equal(t, task.VerdictSkipped, check.Verdict)
	assert.Equal(t, task.EdgeCall, notify.Kind)

	// Explaining must not change anything on disk.
	_, err := os.Stat(filepath.Join(dir, ".task"))
	assert.True(t, os.IsNotExist(err), "explain created the .task directory")

	require.NoError(t, newExplainExecutor(t, dir).Run(t.Context(), &task.Call{Task: "build"}))

	ex = explainOne(t, newExplainExecutor(t, dir), "build")
	assert.Equal(t, task.VerdictUpToDate, ex.Verdict)
	assert.Equal(t, []string{"3 source files unchanged since the last run"}, ex.Reasons)
	assert.Equal(t, task.VerdictUpToDate, ex.Children[0].Verdict, "gen's status check now passes")
	assert.Len(t, ex.Children, 2, "tasks called from cmds are not listed when the task is up to date")
}

func TestExplainReportsChangedFiles(t *testing.T) {
	t.Parallel()

	dir := newExplainProject(t)
	require.NoError(t, newExplainExecutor(t, dir).Run(t.Context(), &task.Call{Task: "build"}))

	writeFile(t, dir, "src/a.txt", "changed")
	writeFile(t, dir, "src/c.txt", "new")
	require.NoError(t, os.Remove(filepath.Join(dir, "src/b.txt")))

	ex := explainOne(t, newExplainExecutor(t, dir), "build")
	assert.Equal(t, task.VerdictWillRun, ex.Verdict)
	assert.Equal(t, []fingerprint.FileChange{
		{Path: "src/a.txt", Change: fingerprint.ChangeModified},
		{Path: "src/b.txt", Change: fingerprint.ChangeRemoved},
		{Path: "src/c.txt", Change: fingerprint.ChangeAdded},
	}, ex.Fingerprint.Changes)
}

func TestExplainTimestamp(t *testing.T) {
	t.Parallel()

	dir := newExplainProject(t)
	require.NoError(t, newExplainExecutor(t, dir).Run(t.Context(), &task.Call{Task: "stamped"}))

	ex := explainOne(t, newExplainExecutor(t, dir), "stamped")
	assert.Equal(t, task.VerdictUpToDate, ex.Verdict)

	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "src/b.txt"), future, future))
	ex = explainOne(t, newExplainExecutor(t, dir), "stamped")
	assert.Equal(t, task.VerdictWillRun, ex.Verdict)
	assert.Equal(t, []fingerprint.FileChange{{Path: "src/b.txt", Change: fingerprint.ChangeNewer}}, ex.Fingerprint.Changes)
}

func TestExplainFailuresAndForce(t *testing.T) {
	t.Parallel()

	dir := newExplainProject(t)

	ex := explainOne(t, newExplainExecutor(t, dir), "needs-var")
	assert.Equal(t, task.VerdictWillFail, ex.Verdict)
	assert.Equal(t, []string{"required variables are not set: TARGET"}, ex.Reasons)

	ex = explainOne(t, newExplainExecutor(t, dir), "guarded")
	assert.Equal(t, task.VerdictWillFail, ex.Verdict)
	assert.Equal(t, []string{"precondition not met: the moon is not full"}, ex.Reasons)

	require.NoError(t, newExplainExecutor(t, dir).Run(t.Context(), &task.Call{Task: "build"}))
	ex = explainOne(t, newExplainExecutor(t, dir, task.WithForceAll(true)), "build")
	assert.Equal(t, task.VerdictWillRun, ex.Verdict)
	assert.Equal(t, []string{"forced with --force"}, ex.Reasons)
}

func TestExplainOutput(t *testing.T) {
	t.Parallel()

	dir := newExplainProject(t)
	e := newExplainExecutor(t, dir)

	var text bytes.Buffer
	require.NoError(t, e.Explain(t.Context(), &text, false, &task.Call{Task: "build"}))
	assert.Equal(t, `build: will run
│   - no previous successful run is recorded
│   - 1 generated file pattern matches nothing
│       missing  out.txt
├── gen: will run
│       - 1 of 1 status checks failed
│           failed   test -f src/gen.txt
├── check: skipped
│       - `+"`if`"+` condition is false: false
└── notify [call]: will run
        - it has no sources or status checks, so it always runs
`, text.String())

	var out bytes.Buffer
	require.NoError(t, e.Explain(t.Context(), &out, true, &task.Call{Task: "build"}))
	var decoded []task.TaskExplanation
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded, 1)
	assert.Equal(t, "build", decoded[0].Task)
	assert.Equal(t, task.VerdictWillRun, decoded[0].Verdict)
}
