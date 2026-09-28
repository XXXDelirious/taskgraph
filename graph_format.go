package task

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Graph output formats accepted by [TaskGraph.Write].
const (
	GraphFormatTree    = "tree"
	GraphFormatDOT     = "dot"
	GraphFormatMermaid = "mermaid"
	GraphFormatJSON    = "json"
)

// GraphFormats lists the supported graph output formats.
var GraphFormats = []string{GraphFormatTree, GraphFormatDOT, GraphFormatMermaid, GraphFormatJSON}

// Write renders the graph in the given format.
func (g *TaskGraph) Write(w io.Writer, format string) error {
	switch format {
	case GraphFormatTree, "":
		return g.WriteTree(w)
	case GraphFormatDOT:
		return g.WriteDOT(w)
	case GraphFormatMermaid:
		return g.WriteMermaid(w)
	case GraphFormatJSON:
		return g.WriteJSON(w)
	default:
		return fmt.Errorf("task: unknown graph format %q (expected one of: %s)", format, strings.Join(GraphFormats, ", "))
	}
}

// WriteJSON renders the graph as indented JSON.
func (g *TaskGraph) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(g)
}

// WriteDOT renders the graph in Graphviz DOT format. Render it with, for
// example, `dot -Tsvg`.
func (g *TaskGraph) WriteDOT(w io.Writer) error {
	var b strings.Builder
	b.WriteString("digraph taskgraph {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box, style=rounded, fontname=\"Helvetica\"];\n")
	b.WriteString("  edge [fontname=\"Helvetica\", fontsize=10];\n")
	for _, n := range g.Nodes {
		var attrs []string
		switch {
		case n.Missing:
			attrs = append(attrs, `color="red"`, `fontcolor="red"`, "label="+strconv.Quote(n.Name+"\n(not found)"))
		case n.Error != "":
			attrs = append(attrs, `color="red"`, "tooltip="+strconv.Quote(n.Error))
		case n.Internal:
			attrs = append(attrs, `style="rounded,dashed"`)
		}
		if n.Desc != "" {
			attrs = append(attrs, "tooltip="+strconv.Quote(n.Desc))
		}
		fmt.Fprintf(&b, "  %s", strconv.Quote(n.Name))
		if len(attrs) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(attrs, ", "))
		}
		b.WriteString(";\n")
	}
	inCycle := g.cycleEdges()
	for _, e := range g.Edges {
		attrs := []string{"label=" + strconv.Quote(e.Kind)}
		if e.Kind == EdgeCall {
			attrs = append(attrs, `style="dashed"`)
		}
		if inCycle[[2]string{e.From, e.To}] {
			attrs = append(attrs, `color="red"`, `fontcolor="red"`, `penwidth=2`)
		}
		fmt.Fprintf(&b, "  %s -> %s [%s];\n", strconv.Quote(e.From), strconv.Quote(e.To), strings.Join(attrs, ", "))
	}
	b.WriteString("}\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMermaid renders the graph as a Mermaid flowchart, which GitHub, GitLab
// and many documentation tools render natively.
func (g *TaskGraph) WriteMermaid(w io.Writer) error {
	ids := make(map[string]string, len(g.Nodes))
	for i, n := range g.Nodes {
		ids[n.Name] = fmt.Sprintf("n%d", i)
	}

	var b strings.Builder
	b.WriteString("flowchart LR\n")
	var missing, internal []string
	for _, n := range g.Nodes {
		label := n.Name
		if n.Missing {
			label += " (not found)"
			missing = append(missing, ids[n.Name])
		} else if n.Internal {
			internal = append(internal, ids[n.Name])
		}
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", ids[n.Name], mermaidEscape(label))
	}
	inCycle := g.cycleEdges()
	var cycleLinks []string
	for i, e := range g.Edges {
		arrow := "-->"
		if e.Kind == EdgeCall {
			arrow = "-.->"
		}
		fmt.Fprintf(&b, "  %s %s|%s| %s\n", ids[e.From], arrow, e.Kind, ids[e.To])
		if inCycle[[2]string{e.From, e.To}] {
			cycleLinks = append(cycleLinks, strconv.Itoa(i))
		}
	}
	if len(missing) > 0 {
		b.WriteString("  classDef missing stroke:#d00,color:#d00\n")
		fmt.Fprintf(&b, "  class %s missing\n", strings.Join(missing, ","))
	}
	if len(internal) > 0 {
		b.WriteString("  classDef internal stroke-dasharray:4 3\n")
		fmt.Fprintf(&b, "  class %s internal\n", strings.Join(internal, ","))
	}
	if len(cycleLinks) > 0 {
		fmt.Fprintf(&b, "  linkStyle %s stroke:#d00,stroke-width:2px\n", strings.Join(cycleLinks, ","))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func mermaidEscape(s string) string {
	return strings.ReplaceAll(s, `"`, "#quot;")
}

// WriteTree renders the graph as an indented tree, starting from the tasks
// nothing else depends on. A task that was already printed is marked with
// "(see above)" instead of being expanded again.
func (g *TaskGraph) WriteTree(w io.Writer) error {
	children := map[string][]*GraphEdge{}
	hasParent := map[string]bool{}
	nodes := map[string]*GraphNode{}
	for _, n := range g.Nodes {
		nodes[n.Name] = n
	}
	for _, e := range g.Edges {
		children[e.From] = append(children[e.From], e)
		hasParent[e.To] = true
	}

	var roots []string
	for _, n := range g.Nodes {
		if !hasParent[n.Name] {
			roots = append(roots, n.Name)
		}
	}
	printed := map[string]bool{}

	var b strings.Builder
	var walk func(name, prefix string, onPath map[string]bool)
	walk = func(name, prefix string, onPath map[string]bool) {
		kids := children[name]
		for i, e := range kids {
			last := i == len(kids)-1
			branch, next := "├── ", "│   "
			if last {
				branch, next = "└── ", "    "
			}
			fmt.Fprintf(&b, "%s%s%s%s", prefix, branch, e.To, treeNodeSuffix(nodes[e.To], e.Kind))
			switch {
			case onPath[e.To]:
				b.WriteString("  ⟲ cycle\n")
			case printed[e.To] && len(children[e.To]) > 0:
				b.WriteString("  (see above)\n")
			default:
				b.WriteString("\n")
				printed[e.To] = true
				onPath[e.To] = true
				walk(e.To, prefix+next, onPath)
				delete(onPath, e.To)
			}
		}
	}
	writeRoot := func(name string) {
		fmt.Fprintf(&b, "%s%s\n", name, treeNodeSuffix(nodes[name], ""))
		printed[name] = true
		walk(name, "", map[string]bool{name: true})
	}
	for _, r := range roots {
		writeRoot(r)
	}
	// Tasks that are only reachable through a cycle have no root; print them
	// too so nothing is left out.
	for _, n := range g.Nodes {
		if !printed[n.Name] {
			writeRoot(n.Name)
		}
	}
	if len(g.Cycles) > 0 {
		b.WriteString("\nCycles:\n")
		for _, c := range g.Cycles {
			fmt.Fprintf(&b, "  %s\n", strings.Join(c, " -> "))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func treeNodeSuffix(n *GraphNode, kind string) string {
	var tags []string
	if kind == EdgeCall {
		tags = append(tags, "call")
	}
	if n != nil {
		switch {
		case n.Missing:
			tags = append(tags, "not found")
		case n.Error != "":
			tags = append(tags, "error: "+n.Error)
		case n.Internal:
			tags = append(tags, "internal")
		}
	}
	if len(tags) == 0 {
		return ""
	}
	return " [" + strings.Join(tags, ", ") + "]"
}
