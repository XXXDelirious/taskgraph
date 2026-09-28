//go:build watch
// +build watch

package task_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/internal/filepathext"
)

func TestFileWatch(t *testing.T) {
	t.Parallel()

	const dir = "testdata/watch"
	_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
	_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))

	expectedOutput := strings.TrimSpace(`
task: Started watching for tasks: default
task: [default] echo "Task running!"
Task running!
task: task "default" finished running
task: [default] echo "Task running!"
Task running!
task: task "default" finished running
	`)

	var buff SyncBuffer
	e := task.NewExecutor(
		task.WithDir(dir),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
		task.WithWatch(true),
	)

	require.NoError(t, e.Setup())
	buff.Reset()

	dirPath := filepathext.SmartJoin(dir, "src")
	filePath := filepathext.SmartJoin(dirPath, "a")

	err := os.MkdirAll(dirPath, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filePath, []byte("test"), 0o644)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				err := e.Run(ctx, &task.Call{Task: "default"})
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	err = os.WriteFile(filePath, []byte("test updated"), 0o644)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	cancel()
	assert.Equal(t, expectedOutput, strings.TrimSpace(buff.String()))
}

func TestFileWatchOnlyRerunsAffectedTask(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, content := range map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  a:
    sources: ["a/*"]
    cmds: ["echo ran-a"]
  b:
    sources: ["b/*"]
    cmds: ["echo ran-b"]
`,
		"a/file": "a",
		"b/file": "b",
	} {
		path := filepathext.SmartJoin(dir, name)
		require.NoError(t, os.MkdirAll(filepathext.SmartJoin(path, ".."), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	var buff SyncBuffer
	e := task.NewExecutor(
		task.WithDir(dir),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
		task.WithWatch(true),
		task.WithSilent(true),
	)
	require.NoError(t, e.Setup())

	go func() {
		_ = e.Run(context.Background(), &task.Call{Task: "a"}, &task.Call{Task: "b"})
	}()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepathext.SmartJoin(dir, "a/file"), []byte("a updated"), 0o644))
	time.Sleep(500 * time.Millisecond)

	out := buff.String()
	assert.Equal(t, 2, strings.Count(out, "ran-a\n"), out)
	assert.Equal(t, 1, strings.Count(out, "ran-b\n"), "a change to a's sources must not re-run b\n%s", out)
}

func TestShouldIgnore(t *testing.T) {
	t.Parallel()

	tt := []struct {
		path   string
		expect bool
	}{
		{"/.git/hooks", true},
		{"/.github/workflows/build.yaml", false},
	}

	for k, ct := range tt {
		ct := ct
		t.Run(fmt.Sprintf("ignore - %d", k), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, task.ShouldIgnore(ct.path), ct.expect)
		})
	}
}
