// Package mcpserver exposes the tasks of a Taskfile as tools over the Model
// Context Protocol (MCP), so coding agents such as Cursor or GitHub Copilot
// can run a project's real build, test and lint steps instead of guessing
// shell commands.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/errors"
	"github.com/XXXDelirious/taskgraph/internal/version"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// DefaultMaxOutputBytes is how much of a task's output is returned to the
// client by default. Longer output is cut from the start, since the end of a
// build or test log is usually what matters.
const DefaultMaxOutputBytes = 64 * 1024

// Names of the built-in tools.
const (
	ExplainToolName = "taskgraph_explain"
	GraphToolName   = "taskgraph_graph"
)

// Options configures a server.
type Options struct {
	// ExecutorOptions are applied to every executor the server creates, e.g.
	// the directory and Taskfile to use. The server sets I/O itself.
	ExecutorOptions []task.ExecutorOption
	// Tasks, if not empty, lists the only tasks to expose. Otherwise every
	// task with a description that is not internal is exposed, unless it sets
	// `mcp: false`.
	Tasks []string
	// MaxOutputBytes caps the output returned for each task run. Zero means
	// [DefaultMaxOutputBytes].
	MaxOutputBytes int
}

// Server serves a Taskfile's tasks over MCP.
type Server struct {
	opts  Options
	mcp   *mcp.Server
	tools []string
}

// New reads the Taskfile and registers one tool per exposed task, plus the
// built-in explain and graph tools.
func New(opts Options) (*Server, error) {
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	s := &Server{opts: opts}

	e, err := s.newExecutor(io.Discard, false)
	if err != nil {
		return nil, err
	}
	tasks, err := s.selectTasks(e)
	if err != nil {
		return nil, err
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "taskgraph",
		Title:   "taskgraph",
		Version: version.GetVersion(),
	}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf(
			"These tools run the tasks defined in the project's Taskfile (%s). "+
				"Prefer them over typing the equivalent shell commands: they run the "+
				"project's own build, test and lint steps, with their dependencies. "+
				"Use %s to check whether a task needs to run and why, and %s to see "+
				"which tasks a task runs.",
			e.Entrypoint, ExplainToolName, GraphToolName),
	})

	names := toolNames(tasks)
	for _, t := range tasks {
		// Listed tasks have their commands templated already, so look for
		// CLI_ARGS in the task as written.
		raw, _ := e.Taskfile.Tasks.Get(t.Task)
		// The task list is compiled without deps, so compile each task fully
		// (without running dynamic variables) to describe it.
		compiled, err := e.FastCompiledTask(&task.Call{Task: t.Task})
		if err != nil {
			return nil, err
		}
		tool := taskTool(names[t.Task], compiled, raw != nil && usesCLIArgs(raw))
		s.mcp.AddTool(tool, s.runTaskHandler(t.Task, compiled.Requires))
		s.tools = append(s.tools, tool.Name)
	}
	s.addBuiltinTools()
	return s, nil
}

// Tools returns the names of the registered tools.
func (s *Server) Tools() []string {
	return slices.Clone(s.tools)
}

// MCPServer returns the underlying MCP server, e.g. to connect it to a
// transport other than stdio.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcp
}

// Run serves MCP over stdin and stdout until the client disconnects or ctx is
// cancelled.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// newExecutor creates a ready-to-use executor that writes all output to w.
// Stdin is always empty: stdin belongs to the MCP connection, and tasks
// cannot prompt an agent.
func (s *Server) newExecutor(w io.Writer, forceAll bool) (*task.Executor, error) {
	opts := slices.Clone(s.opts.ExecutorOptions)
	opts = append(opts,
		task.WithStdin(strings.NewReader("")),
		task.WithStdout(w),
		task.WithStderr(w),
		task.WithColor(false),
		task.WithInteractive(false),
	)
	if forceAll {
		opts = append(opts, task.WithForceAll(true))
	}
	e := task.NewExecutor(opts...)
	if err := e.Setup(); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Server) selectTasks(e *task.Executor) ([]*ast.Task, error) {
	all, err := e.GetTaskList()
	if err != nil {
		return nil, err
	}

	if len(s.opts.Tasks) > 0 {
		var selected []*ast.Task
		for _, name := range s.opts.Tasks {
			t, err := e.GetTask(&task.Call{Task: name})
			if err != nil {
				return nil, err
			}
			if t.Internal {
				return nil, &errors.TaskInternalError{TaskName: name}
			}
			idx := slices.IndexFunc(all, func(c *ast.Task) bool { return c.Task == t.Task })
			if idx >= 0 && !slices.Contains(selected, all[idx]) {
				selected = append(selected, all[idx])
			}
		}
		return selected, nil
	}

	var selected []*ast.Task
	for _, t := range all {
		if t.Internal || strings.Contains(t.Task, "*") {
			continue
		}
		expose := t.Desc != ""
		if t.MCP != nil && t.MCP.Expose != nil {
			expose = *t.MCP.Expose
		}
		if expose {
			selected = append(selected, t)
		}
	}
	return selected, nil
}

// toolNames maps task names to valid, unique MCP tool names. Tool names may
// only contain letters, digits, '_' and '-' in many clients, so namespace
// separators (':') and other characters become '_'.
func toolNames(tasks []*ast.Task) map[string]string {
	names := make(map[string]string, len(tasks))
	used := map[string]bool{ExplainToolName: true, GraphToolName: true}
	for _, t := range tasks {
		var b strings.Builder
		for _, r := range t.Task {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		base := b.String()
		if len(base) > 60 {
			base = base[:60]
		}
		name := base
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		used[name] = true
		names[t.Task] = name
	}
	return names
}

// Reserved argument names. A required variable with the same name takes
// precedence, and the extra argument is left out.
const (
	argVars    = "vars"
	argCLIArgs = "cli_args"
	argForce   = "force"
)

func taskTool(name string, t *ast.Task, cliArgs bool) *mcp.Tool {
	properties := map[string]any{}
	required := []string{}
	if t.Requires != nil {
		for _, v := range t.Requires.Vars {
			prop := map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Required variable %s.", v.Name),
			}
			if v.Enum != nil && len(v.Enum.Value) > 0 {
				prop["enum"] = v.Enum.Value
			}
			properties[v.Name] = prop
			required = append(required, v.Name)
		}
	}
	addProperty := func(key string, prop map[string]any) {
		if _, ok := properties[key]; !ok {
			properties[key] = prop
		}
	}
	addProperty(argVars, map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          "Other variables to set, like NAME=value on the command line.",
	})
	if cliArgs {
		addProperty(argCLIArgs, map[string]any{
			"type":        "string",
			"description": "Extra arguments for the task, available to it as CLI_ARGS (what would follow -- on the command line).",
		})
	}
	addProperty(argForce, map[string]any{
		"type":        "boolean",
		"description": "Run the task and its dependencies even if they are up to date.",
	})

	annotations := &mcp.ToolAnnotations{Title: t.Task}
	if t.MCP != nil {
		annotations.ReadOnlyHint = t.MCP.ReadOnly
		annotations.IdempotentHint = t.MCP.Idempotent
		destructive := t.MCP.Destructive
		annotations.DestructiveHint = &destructive
	}

	return &mcp.Tool{
		Name:        name,
		Title:       t.Task,
		Description: taskDescription(t),
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		},
		Annotations: annotations,
	}
}

func usesCLIArgs(t *ast.Task) bool {
	for _, c := range t.Cmds {
		if strings.Contains(c.Cmd, "CLI_ARGS") {
			return true
		}
	}
	return false
}

func taskDescription(t *ast.Task) string {
	var parts []string
	if t.Desc != "" {
		parts = append(parts, strings.TrimSpace(t.Desc))
	} else {
		parts = append(parts, fmt.Sprintf("Runs the %q task.", t.Task))
	}
	if s := strings.TrimSpace(t.Summary); s != "" {
		parts = append(parts, s)
	}
	var deps []string
	for _, d := range t.Deps {
		if d.Task != "" {
			deps = append(deps, d.Task)
		}
	}
	if len(deps) > 0 {
		parts = append(parts, "First runs its dependencies: "+strings.Join(deps, ", ")+".")
	}
	if len(t.Sources) > 0 || len(t.Status) > 0 {
		parts = append(parts, "Does nothing if it is already up to date, unless force is set.")
	}
	if len(t.Prompt) > 0 {
		parts = append(parts, "Asks for confirmation before running, so it will be cancelled when run from here; ask the user to run it.")
	}
	return strings.Join(parts, "\n\n")
}

// RunResult is the structured result of running a task.
type RunResult struct {
	Task       string `json:"task"`
	Success    bool   `json:"success"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output"`
	// Truncated is set when the start of the output was cut to fit
	// MaxOutputBytes.
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) runTaskHandler(taskName string, requires *ast.Requires) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs(req.Params.Arguments, requires)
		if err != nil {
			return toolError(err), nil
		}

		out := &tailBuffer{max: s.opts.MaxOutputBytes}
		e, err := s.newExecutor(out, args.force)
		if err != nil {
			return toolError(err), nil
		}
		e.Taskfile.Vars.Merge(args.vars, nil)
		special := ast.NewVars()
		special.Set("CLI_ARGS", ast.Var{Value: args.cliArgs})
		special.Set("CLI_ARGS_LIST", ast.Var{Value: strings.Fields(args.cliArgs)})
		special.Set("CLI_FORCE", ast.Var{Value: args.force})
		special.Set("CLI_SILENT", ast.Var{Value: e.Silent})
		special.Set("CLI_VERBOSE", ast.Var{Value: e.Verbose})
		special.Set("CLI_OFFLINE", ast.Var{Value: e.Offline})
		special.Set("CLI_ASSUME_YES", ast.Var{Value: false})
		e.Taskfile.Vars.ReverseMerge(special, nil)

		start := time.Now()
		runErr := e.Run(ctx, &task.Call{Task: taskName})
		output, truncated := out.String()
		result := RunResult{
			Task:       taskName,
			Success:    runErr == nil,
			DurationMs: time.Since(start).Milliseconds(),
			Output:     output,
			Truncated:  truncated,
		}
		if runErr != nil {
			result.Error = runErr.Error()
			result.ExitCode = exitCode(runErr)
		}

		var text strings.Builder
		if runErr == nil {
			fmt.Fprintf(&text, "Task %q succeeded in %s.\n", taskName, time.Duration(result.DurationMs)*time.Millisecond)
		} else {
			fmt.Fprintf(&text, "Task %q failed (exit code %d): %s\n", taskName, result.ExitCode, result.Error)
		}
		if truncated {
			fmt.Fprintf(&text, "(showing the last %d bytes of output)\n", s.opts.MaxOutputBytes)
		}
		if output != "" {
			text.WriteString("\n")
			text.WriteString(output)
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: text.String()}},
			StructuredContent: result,
			IsError:           runErr != nil,
		}, nil
	}
}

type runArgs struct {
	vars    *ast.Vars
	cliArgs string
	force   bool
}

func parseArgs(raw json.RawMessage, requires *ast.Requires) (*runArgs, error) {
	args := &runArgs{vars: ast.NewVars()}
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	requiredNames := map[string]bool{}
	if requires != nil {
		for _, v := range requires.Vars {
			requiredNames[v.Name] = true
		}
	}

	for k, v := range m {
		switch {
		case requiredNames[k]:
			args.vars.Set(k, ast.Var{Value: fmt.Sprint(v)})
		case k == argVars:
			extra, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%q must be an object of strings", argVars)
			}
			for name, value := range extra {
				args.vars.Set(name, ast.Var{Value: fmt.Sprint(value)})
			}
		case k == argCLIArgs:
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%q must be a string", argCLIArgs)
			}
			args.cliArgs = str
		case k == argForce:
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%q must be a boolean", argForce)
			}
			args.force = b
		default:
			return nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	return args, nil
}

func exitCode(err error) int {
	var runErr *errors.TaskRunError
	if errors.As(err, &runErr) {
		return runErr.TaskExitCode()
	}
	var taskErr errors.TaskError
	if errors.As(err, &taskErr) {
		return taskErr.Code()
	}
	return errors.CodeUnknown
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		IsError: true,
	}
}

func (s *Server) addBuiltinTools() {
	tasksProperty := map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": "Task names, as written in the Taskfile (e.g. \"docs:build\").",
	}
	varsProperty := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          "Variables to set, as when running the tasks.",
	}

	s.mcp.AddTool(&mcp.Tool{
		Name:  ExplainToolName,
		Title: "Explain tasks",
		Description: "Says whether each task, and every task it runs, would run and why, " +
			"without running any task commands: which source files changed, which status " +
			"checks fail, missing generated files, unmet preconditions, missing variables.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"tasks": withMinItems(tasksProperty), "vars": varsProperty},
			"required":             []string{"tasks"},
			"additionalProperties": false,
		},
		Annotations: &mcp.ToolAnnotations{Title: "Explain tasks", ReadOnlyHint: true, IdempotentHint: true},
	}, s.explainHandler)
	s.tools = append(s.tools, ExplainToolName)

	s.mcp.AddTool(&mcp.Tool{
		Name:  GraphToolName,
		Title: "Task graph",
		Description: "Returns the graph of tasks and the tasks they run (deps and calls), " +
			"including missing tasks and cycles. Leave tasks empty for the whole Taskfile.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"tasks": tasksProperty, "vars": varsProperty},
			"additionalProperties": false,
		},
		Annotations: &mcp.ToolAnnotations{Title: "Task graph", ReadOnlyHint: true, IdempotentHint: true},
	}, s.graphHandler)
	s.tools = append(s.tools, GraphToolName)
}

func withMinItems(prop map[string]any) map[string]any {
	c := maps.Clone(prop)
	c["minItems"] = 1
	return c
}

func parseTaskList(raw json.RawMessage) ([]*task.Call, *ast.Vars, error) {
	var args struct {
		Tasks []string          `json:"tasks"`
		Vars  map[string]string `json:"vars"`
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return nil, nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	calls := make([]*task.Call, 0, len(args.Tasks))
	for _, name := range args.Tasks {
		calls = append(calls, &task.Call{Task: name})
	}
	vars := ast.NewVars()
	for k, v := range args.Vars {
		vars.Set(k, ast.Var{Value: v})
	}
	return calls, vars, nil
}

func (s *Server) explainHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	calls, vars, err := parseTaskList(req.Params.Arguments)
	if err != nil {
		return toolError(err), nil
	}
	if len(calls) == 0 {
		return toolError(fmt.Errorf("give at least one task")), nil
	}
	e, err := s.newExecutor(io.Discard, false)
	if err != nil {
		return toolError(err), nil
	}
	e.Taskfile.Vars.Merge(vars, nil)
	explanations, err := e.ExplainTasks(ctx, calls...)
	if err != nil {
		return toolError(err), nil
	}
	var text strings.Builder
	if err := e.Explain(ctx, &text, false, calls...); err != nil {
		return toolError(err), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text.String()}},
		StructuredContent: map[string]any{"tasks": explanations},
	}, nil
}

func (s *Server) graphHandler(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	calls, vars, err := parseTaskList(req.Params.Arguments)
	if err != nil {
		return toolError(err), nil
	}
	e, err := s.newExecutor(io.Discard, false)
	if err != nil {
		return toolError(err), nil
	}
	e.Taskfile.Vars.Merge(vars, nil)
	g, err := e.TaskGraph(calls...)
	if err != nil {
		return toolError(err), nil
	}
	var text strings.Builder
	if err := g.WriteTree(&text); err != nil {
		return toolError(err), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text.String()}},
		StructuredContent: g,
	}, nil
}

// tailBuffer keeps the last max bytes written to it. It is safe for
// concurrent use, since a task's dependencies run in parallel.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	// Trim in batches so long outputs are not copied on every write.
	if len(b.buf) > 2*b.max {
		b.buf = append(b.buf[:0:0], b.buf[len(b.buf)-b.max:]...)
		b.truncated = true
	}
	return len(p), nil
}

// String returns the kept output, and whether earlier output was dropped.
func (b *tailBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) > b.max {
		return string(b.buf[len(b.buf)-b.max:]), true
	}
	return string(b.buf), b.truncated
}
