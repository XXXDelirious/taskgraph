package task_test

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	task "github.com/XXXDelirious/taskgraph"
)

func newGraphExecutor(t *testing.T) *task.Executor {
	t.Helper()
	e := task.NewExecutor(
		task.WithDir("testdata/graph"),
		task.WithStdout(io.Discard),
		task.WithStderr(io.Discard),
	)
	require.NoError(t, e.Setup())
	return e
}

func edgeStrings(g *task.TaskGraph) []string {
	edges := make([]string, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, e.From+" -"+e.Kind+"-> "+e.To)
	}
	return edges
}

func TestTaskGraphFromCall(t *testing.T) {
	t.Parallel()

	e := newGraphExecutor(t)
	g, err := e.TaskGraph(&task.Call{Task: "test"})
	require.NoError(t, err)

	names := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		names = append(names, n.Name)
	}
	assert.Equal(t, []string{"test", "build", "gen", "lint", "report", "deploy:staging"}, names)
	assert.Equal(t, []string{
		"test -dep-> build",
		"test -call-> report",
		"test -call-> deploy:staging",
		"build -dep-> gen",
		"build -dep-> lint",
	}, edgeStrings(g))
	assert.Empty(t, g.Cycles)

	build := g.Nodes[1]
	assert.Equal(t, "Build the app", build.Desc)
	assert.Equal(t, []string{"src/**/*.go"}, build.Sources)
	assert.Equal(t, []string{"bin/app"}, build.Generates)
	assert.True(t, g.Nodes[3].Internal)
}

func TestTaskGraphMissingTaskAndCycles(t *testing.T) {
	t.Parallel()

	e := newGraphExecutor(t)
	g, err := e.TaskGraph()
	require.NoError(t, err)

	var missing []string
	for _, n := range g.Nodes {
		if n.Missing {
			missing = append(missing, n.Name)
		}
	}
	assert.Equal(t, []string{"does-not-exist"}, missing)
	assert.Equal(t, [][]string{{"ping", "pong", "ping"}}, g.Cycles)
}

func TestTaskGraphFormats(t *testing.T) {
	t.Parallel()

	e := newGraphExecutor(t)
	g, err := e.TaskGraph()
	require.NoError(t, err)

	gold := goldie.New(t, goldie.WithFixtureDir(filepath.Join("testdata", "graph", "testdata")))
	for _, format := range task.GraphFormats {
		var buf bytes.Buffer
		require.NoError(t, g.Write(&buf, format))
		gold.Assert(t, "graph-"+format, buf.Bytes())
	}

	var decoded task.TaskGraph
	var buf bytes.Buffer
	require.NoError(t, g.WriteJSON(&buf))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Len(t, decoded.Nodes, len(g.Nodes))

	assert.ErrorContains(t, g.Write(io.Discard, "svg"), `unknown graph format "svg"`)
}
