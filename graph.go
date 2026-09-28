package task

import (
	"cmp"
	"slices"
	"strings"

	"github.com/XXXDelirious/taskgraph/errors"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// Edge kinds in a [TaskGraph].
const (
	// EdgeDep is a task listed under `deps:`. Deps run in parallel, before the
	// task's own commands.
	EdgeDep = "dep"
	// EdgeCall is a task called from `cmds:` with `task:`. Calls run in
	// order, as part of the task's commands.
	EdgeCall = "call"
)

// TaskGraph is the graph of tasks and the tasks they run. Edges point from a
// task to the tasks it runs.
type TaskGraph struct {
	Nodes []*GraphNode `json:"nodes"`
	Edges []*GraphEdge `json:"edges"`
	// Cycles lists every cycle found in the graph. Each cycle starts and ends
	// with the same task.
	Cycles [][]string `json:"cycles"`
}

// GraphNode is a task in a [TaskGraph].
type GraphNode struct {
	Name      string   `json:"name"`
	Desc      string   `json:"desc,omitempty"`
	Internal  bool     `json:"internal,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Generates []string `json:"generates,omitempty"`
	// Missing is set when another task refers to this name but no such task
	// exists.
	Missing bool `json:"missing,omitempty"`
	// Error is set when the task could not be compiled, e.g. because of a
	// template error.
	Error string `json:"error,omitempty"`
}

// GraphEdge is a dependency between two tasks in a [TaskGraph].
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is either [EdgeDep] or [EdgeCall].
	Kind string `json:"kind"`
}

// TaskGraph builds the graph of tasks reachable from the given calls. With no
// calls, it includes every task in the Taskfile.
//
// Task names that come from dynamic (sh) variables are not resolved, because
// building the graph never runs commands.
func (e *Executor) TaskGraph(calls ...*Call) (*TaskGraph, error) {
	g, _, err := e.buildTaskGraph(e.FastCompiledTask, calls...)
	return g, err
}

// buildTaskGraph builds the graph using compile to compile each task, and
// also returns the compiled task for each node that has one.
func (e *Executor) buildTaskGraph(compile func(*Call) (*ast.Task, error), calls ...*Call) (*TaskGraph, map[string]*ast.Task, error) {
	b := &graphBuilder{
		compile: compile,
		nodes:   map[string]*GraphNode{},
		tasks:   map[string]*ast.Task{},
		edges:   map[GraphEdge]bool{},
		out:     map[string][]string{},
	}

	if len(calls) == 0 {
		for name := range e.Taskfile.Tasks.Keys(e.TaskSorter) {
			calls = append(calls, &Call{Task: name})
		}
	}
	for _, c := range calls {
		if _, err := b.visit(c); err != nil {
			return nil, nil, err
		}
	}

	g := &TaskGraph{Nodes: make([]*GraphNode, 0, len(b.nodes)), Edges: make([]*GraphEdge, 0, len(b.edges))}
	for _, name := range b.order {
		g.Nodes = append(g.Nodes, b.nodes[name])
	}
	// Edges are added as the search returns, so list them in node order to
	// keep each task's edges together, in the order they were declared.
	position := make(map[string]int, len(b.order))
	for i, name := range b.order {
		position[name] = i
	}
	g.Edges = append(g.Edges, b.edgeOrder...)
	slices.SortStableFunc(g.Edges, func(x, y *GraphEdge) int {
		return cmp.Compare(position[x.From], position[y.From])
	})
	g.Cycles = findGraphCycles(b.order, b.out)
	return g, b.tasks, nil
}

type graphBuilder struct {
	compile   func(*Call) (*ast.Task, error)
	nodes     map[string]*GraphNode
	tasks     map[string]*ast.Task
	order     []string
	edges     map[GraphEdge]bool
	edgeOrder []*GraphEdge
	out       map[string][]string
}

// visit adds the called task and everything it runs to the graph, and returns
// the task's node name.
func (b *graphBuilder) visit(call *Call) (string, error) {
	t, err := b.compile(call)
	if err != nil {
		var notFound *errors.TaskNotFoundError
		if errors.As(err, &notFound) {
			return b.addNode(&GraphNode{Name: call.Task, Missing: true}), nil
		}
		return b.addNode(&GraphNode{Name: call.Task, Error: err.Error()}), nil
	}

	name := graphNodeName(t, call)
	if _, ok := b.nodes[name]; ok {
		return name, nil
	}
	b.tasks[name] = t
	b.addNode(&GraphNode{
		Name:      name,
		Desc:      t.Desc,
		Internal:  t.Internal,
		Sources:   globPatterns(t.Sources),
		Generates: globPatterns(t.Generates),
	})

	for _, d := range t.Deps {
		if d.Task == "" {
			continue
		}
		to, err := b.visit(&Call{Task: d.Task, Vars: d.Vars})
		if err != nil {
			return "", err
		}
		b.addEdge(name, to, EdgeDep)
	}
	for _, c := range t.Cmds {
		if c.Task == "" {
			continue
		}
		to, err := b.visit(&Call{Task: c.Task, Vars: c.Vars})
		if err != nil {
			return "", err
		}
		b.addEdge(name, to, EdgeCall)
	}
	return name, nil
}

// graphNodeName is the name of the graph node for a call to t. Wildcard tasks
// are shown under the name they were called with; other tasks under their
// own name, even when called through an alias.
func graphNodeName(t *ast.Task, call *Call) string {
	if strings.Contains(t.Task, "*") {
		return call.Task
	}
	return t.Task
}

func (b *graphBuilder) addNode(n *GraphNode) string {
	if _, ok := b.nodes[n.Name]; !ok {
		b.nodes[n.Name] = n
		b.order = append(b.order, n.Name)
	}
	return n.Name
}

func (b *graphBuilder) addEdge(from, to, kind string) {
	edge := GraphEdge{From: from, To: to, Kind: kind}
	if b.edges[edge] {
		return
	}
	b.edges[edge] = true
	b.edgeOrder = append(b.edgeOrder, &edge)
	if !slices.Contains(b.out[from], to) {
		b.out[from] = append(b.out[from], to)
	}
}

func globPatterns(globs []*ast.Glob) []string {
	patterns := make([]string, 0, len(globs))
	for _, g := range globs {
		if g.Negate {
			patterns = append(patterns, "!"+g.Glob)
		} else {
			patterns = append(patterns, g.Glob)
		}
	}
	return patterns
}

// findGraphCycles returns one cycle for every back edge found by a depth-first
// search. Each cycle is rotated to start at its alphabetically smallest task,
// so the same cycle is reported only once.
func findGraphCycles(order []string, out map[string][]string) [][]string {
	const (
		unvisited = iota
		inProgress
		done
	)
	state := map[string]int{}
	var stack []string
	var cycles [][]string
	seen := map[string]bool{}

	var dfs func(n string)
	dfs = func(n string) {
		state[n] = inProgress
		stack = append(stack, n)
		for _, next := range out[n] {
			switch state[next] {
			case unvisited:
				dfs(next)
			case inProgress:
				start := slices.Index(stack, next)
				cycle := rotateCycle(slices.Clone(stack[start:]))
				key := strings.Join(cycle, "\x00")
				if !seen[key] {
					seen[key] = true
					cycles = append(cycles, append(cycle, cycle[0]))
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
	}
	for _, n := range order {
		if state[n] == unvisited {
			dfs(n)
		}
	}
	slices.SortFunc(cycles, func(a, b []string) int {
		return cmp.Compare(strings.Join(a, "\x00"), strings.Join(b, "\x00"))
	})
	return cycles
}

func rotateCycle(cycle []string) []string {
	minIdx := 0
	for i, n := range cycle {
		if n < cycle[minIdx] {
			minIdx = i
		}
	}
	return append(cycle[minIdx:], cycle[:minIdx]...)
}

// cycleEdges returns the set of edges (from -> to) that are part of a cycle.
func (g *TaskGraph) cycleEdges() map[[2]string]bool {
	edges := map[[2]string]bool{}
	for _, c := range g.Cycles {
		for i := 0; i+1 < len(c); i++ {
			edges[[2]string{c[i], c[i+1]}] = true
		}
	}
	return edges
}
