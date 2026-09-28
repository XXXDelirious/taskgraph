package mcpserver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// connect starts a server for testdata/Taskfile.yml and returns a client
// session connected to it in memory.
func connect(t *testing.T, opts Options) *mcp.ClientSession {
	t.Helper()
	opts.ExecutorOptions = append(opts.ExecutorOptions, task.WithDir("testdata"))
	s, err := New(opts)
	require.NoError(t, err)

	ctx := t.Context()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := s.MCPServer().Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func listTools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return res, text.Text
}

func schema(tool *mcp.Tool) map[string]any {
	return tool.InputSchema.(map[string]any)
}

func TestToolsList(t *testing.T) {
	t.Parallel()

	tools := listTools(t, connect(t, Options{}))
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	assert.Equal(t, []string{
		"build", "deploy", "docs_build", "exposed-anyway", "fail", "gen", "noisy",
		"taskgraph_explain", "taskgraph_graph", "test",
	}, names, "undocumented, internal and `mcp: false` tasks are hidden")

	build := tools["build"]
	assert.Equal(t, "build", build.Title)
	assert.Contains(t, build.Description, "Build the app")
	assert.Contains(t, build.Description, "Compiles everything under src/.")
	assert.Contains(t, build.Description, "First runs its dependencies: gen.")
	props := schema(build)["properties"].(map[string]any)
	assert.Equal(t, []any{"TARGET"}, schema(build)["required"])
	assert.Equal(t, []any{"linux", "darwin"}, props["TARGET"].(map[string]any)["enum"])
	assert.Contains(t, props, "vars")
	assert.Contains(t, props, "force")
	assert.NotContains(t, props, "cli_args", "build does not use CLI_ARGS")

	assert.Contains(t, schema(tools["test"])["properties"], "cli_args")
	assert.Contains(t, tools["deploy"].Description, "ask the user to run it")

	gen := tools["gen"].Annotations
	assert.True(t, gen.IdempotentHint)
	require.NotNil(t, gen.DestructiveHint)
	assert.False(t, *gen.DestructiveHint)
	assert.True(t, tools["taskgraph_explain"].Annotations.ReadOnlyHint)
}

func TestToolsAllowlist(t *testing.T) {
	t.Parallel()

	tools := listTools(t, connect(t, Options{Tasks: []string{"test", "docs:serve", "undocumented"}}))
	assert.Len(t, tools, 5)
	assert.Contains(t, tools, "test")
	assert.Contains(t, tools, "docs_serve", "an allowlist can expose tasks hidden by default")
	assert.Contains(t, tools, "undocumented")

	_, err := New(Options{
		ExecutorOptions: []task.ExecutorOption{task.WithDir("testdata")},
		Tasks:           []string{"helper"},
	})
	assert.ErrorContains(t, err, "internal")
}

func TestRunTask(t *testing.T) {
	t.Parallel()

	cs := connect(t, Options{})
	res, text := call(t, cs, "build", map[string]any{
		"TARGET": "linux",
		"vars":   map[string]any{"EXTRA": "fast"},
	})
	assert.False(t, res.IsError)
	assert.Contains(t, text, `Task "build" succeeded`)
	assert.Contains(t, text, "built for linux fast")
	assert.Contains(t, text, "task: [gen] echo gen", "dependencies run too")

	var result RunResult
	data, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &result))
	assert.True(t, result.Success)
	assert.Equal(t, "build", result.Task)
	assert.Equal(t, 0, result.ExitCode)

	_, text = call(t, cs, "test", map[string]any{"cli_args": "-run TestFoo"})
	assert.Contains(t, text, "testing -run TestFoo")
}

func TestRunTaskFailure(t *testing.T) {
	t.Parallel()

	cs := connect(t, Options{})
	res, text := call(t, cs, "fail", nil)
	assert.True(t, res.IsError)
	assert.Contains(t, text, `Task "fail" failed (exit code 3)`)
	assert.Contains(t, text, "about to fail")

	res, text = call(t, cs, "build", map[string]any{"TARGET": "windows"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "TARGET", "values outside the enum are rejected")

	res, text = call(t, cs, "build", map[string]any{"TARGET": "linux", "nope": "1"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, `unknown argument "nope"`)

	res, text = call(t, cs, "deploy", nil)
	assert.True(t, res.IsError, "tasks with a prompt are cancelled, not confirmed")
	assert.NotContains(t, text, "deploying")
}

func TestRunTaskTruncatesOutput(t *testing.T) {
	t.Parallel()

	cs := connect(t, Options{MaxOutputBytes: 1000})
	res, text := call(t, cs, "noisy", nil)
	assert.False(t, res.IsError)
	assert.Contains(t, text, "showing the last 1000 bytes")
	assert.Contains(t, text, "line 2000")
	assert.NotContains(t, text, "line 1\n")
}

func TestExplainAndGraphTools(t *testing.T) {
	t.Parallel()

	cs := connect(t, Options{})
	res, text := call(t, cs, ExplainToolName, map[string]any{
		"tasks": []string{"build"},
		"vars":  map[string]string{"TARGET": "linux"},
	})
	assert.False(t, res.IsError)
	assert.Contains(t, text, "build: will run")
	assert.Contains(t, text, "gen: will run")

	res, _ = call(t, cs, ExplainToolName, map[string]any{"tasks": []string{}})
	assert.True(t, res.IsError)

	res, text = call(t, cs, GraphToolName, map[string]any{"tasks": []string{"build"}})
	assert.False(t, res.IsError)
	assert.Equal(t, "build\n└── gen\n", text)
	data, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var g task.TaskGraph
	require.NoError(t, json.Unmarshal(data, &g))
	assert.Len(t, g.Nodes, 2)
}

func TestToolNames(t *testing.T) {
	t.Parallel()

	names := toolNames([]*ast.Task{
		{Task: "docs:build"},
		{Task: "docs_build"},
		{Task: "taskgraph_explain"},
		{Task: "deploy.prod"},
		{Task: strings.Repeat("x", 80)},
	})
	assert.Equal(t, "docs_build", names["docs:build"])
	assert.Equal(t, "docs_build_2", names["docs_build"])
	assert.Equal(t, "taskgraph_explain_2", names["taskgraph_explain"], "built-in tool names are reserved")
	assert.Equal(t, "deploy_prod", names["deploy.prod"])
	assert.Len(t, names[strings.Repeat("x", 80)], 60)
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()

	b := &tailBuffer{max: 5}
	_, _ = b.Write([]byte("abc"))
	out, truncated := b.String()
	assert.Equal(t, "abc", out)
	assert.False(t, truncated)

	_, _ = b.Write([]byte("defghijklmn"))
	out, truncated = b.String()
	assert.Equal(t, "jklmn", out)
	assert.True(t, truncated)
}

func TestServerInstructions(t *testing.T) {
	t.Parallel()

	cs := connect(t, Options{})
	res := cs.InitializeResult()
	require.NotNil(t, res)
	assert.Contains(t, res.Instructions, ExplainToolName)
	assert.Equal(t, "taskgraph", res.ServerInfo.Name)
}
