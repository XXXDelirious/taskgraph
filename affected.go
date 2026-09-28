package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/pattern"

	"github.com/XXXDelirious/taskgraph/internal/filepathext"
	"github.com/XXXDelirious/taskgraph/internal/fingerprint"
	"github.com/XXXDelirious/taskgraph/internal/vcs"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// AffectedTask is a task affected by a set of changed files.
type AffectedTask struct {
	Task string `json:"task"`
	// Files are the changed files matched by the task's own sources, relative
	// to the root Taskfile's directory.
	Files []string `json:"files,omitempty"`
	// TaskfileChanged is set when the Taskfile that defines the task changed.
	TaskfileChanged bool `json:"taskfileChanged,omitempty"`
	// Via lists the affected tasks that this task runs, through deps or
	// calls.
	Via []string `json:"via,omitempty"`
}

// ChangedFiles returns the files changed in the git repository that contains
// the Taskfile: uncommitted and untracked changes, plus, when since is set,
// everything committed since the merge base of since and HEAD.
//
// Files in taskgraph's own state directories (.task/ by default) are left
// out, in case they are not ignored by git.
func (e *Executor) ChangedFiles(ctx context.Context, since string) ([]string, error) {
	files, err := vcs.ChangedFiles(ctx, e.Dir, since)
	if err != nil {
		return nil, err
	}
	var stateDirs []string
	for _, dir := range []string{e.TempDir.Fingerprint, e.TempDir.Remote} {
		if dir != "" {
			stateDirs = append(stateDirs, realPath(dir)+string(filepath.Separator))
		}
	}
	return slices.DeleteFunc(files, func(f string) bool {
		real := realPath(f)
		return slices.ContainsFunc(stateDirs, func(dir string) bool {
			return strings.HasPrefix(real, dir)
		})
	}), nil
}

// AffectedTasks returns the tasks, among those reachable from calls (or all
// tasks when calls is empty), that are affected by the changed files. A task
// is affected when a changed file matches its sources, when its Taskfile
// changed, or when a task it runs is affected. Tasks are returned in graph
// order.
//
// Tasks without sources are only affected through their Taskfile or the
// tasks they run, so declare sources for tasks that should take part.
func (e *Executor) AffectedTasks(changed []string, calls ...*Call) ([]*AffectedTask, error) {
	g, tasks, err := e.buildTaskGraph(e.CompiledTask, calls...)
	if err != nil {
		return nil, err
	}

	changedSet := make(map[string]bool, len(changed))
	var deleted []string
	for _, f := range changed {
		real := realPath(f)
		changedSet[real] = true
		if _, err := os.Stat(f); os.IsNotExist(err) {
			deleted = append(deleted, real)
		}
	}

	direct := map[string]*AffectedTask{}
	for _, n := range g.Nodes {
		t := tasks[n.Name]
		if t == nil {
			continue
		}
		a, err := e.directlyAffected(t, n.Name, changedSet, deleted)
		if err != nil {
			return nil, err
		}
		if a != nil {
			direct[n.Name] = a
		}
	}

	out := map[string][]string{}
	for _, edge := range g.Edges {
		out[edge.From] = append(out[edge.From], edge.To)
	}

	// A task is affected if it is directly affected or runs an affected task.
	// Tasks on a cycle count as unaffected through the cycle itself.
	affected := map[string]bool{}
	state := map[string]int{}
	var visit func(name string) bool
	visit = func(name string) bool {
		switch state[name] {
		case 1:
			return false
		case 2:
			return affected[name]
		}
		state[name] = 1
		result := direct[name] != nil
		for _, next := range out[name] {
			if visit(next) {
				result = true
			}
		}
		state[name] = 2
		affected[name] = result
		return result
	}

	var result []*AffectedTask
	for _, n := range g.Nodes {
		if !visit(n.Name) {
			continue
		}
		a := direct[n.Name]
		if a == nil {
			a = &AffectedTask{Task: n.Name}
		}
		for _, next := range out[n.Name] {
			if affected[next] && !slices.Contains(a.Via, next) {
				a.Via = append(a.Via, next)
			}
		}
		result = append(result, a)
	}
	return result, nil
}

// AffectedCall reports whether the called task is affected by the changed
// files, and why. It returns nil if it is not.
func (e *Executor) AffectedCall(changed []string, call *Call) (*AffectedTask, error) {
	t, err := e.FastCompiledTask(call)
	if err != nil {
		return nil, err
	}
	name := graphNodeName(t, call)
	affected, err := e.AffectedTasks(changed, call)
	if err != nil {
		return nil, err
	}
	for _, a := range affected {
		if a.Task == name {
			return a, nil
		}
	}
	return nil, nil
}

func (e *Executor) directlyAffected(t *ast.Task, name string, changed map[string]bool, deleted []string) (*AffectedTask, error) {
	a := &AffectedTask{Task: name}
	if t.Location != nil && t.Location.Taskfile != "" && changed[realPath(t.Location.Taskfile)] {
		a.TaskfileChanged = true
	}

	if len(t.Sources) > 0 {
		sources, err := fingerprint.Globs(t.Dir, t.Sources)
		if err != nil {
			return nil, err
		}
		for _, f := range sources {
			if real := realPath(f); changed[real] {
				a.Files = append(a.Files, e.relativeToRoot(real))
			}
		}

		// Deleted files no longer match when the globs are expanded, so match
		// them against the patterns instead.
		if len(deleted) > 0 {
			matchers, err := sourceMatchers(t)
			if err != nil {
				return nil, err
			}
			for _, f := range deleted {
				if matchesSources(matchers, filepath.ToSlash(f)) {
					a.Files = append(a.Files, e.relativeToRoot(f))
				}
			}
		}
		slices.Sort(a.Files)
		a.Files = slices.Compact(a.Files)
	}

	if len(a.Files) == 0 && !a.TaskfileChanged {
		return nil, nil
	}
	return a, nil
}

type sourceMatcher struct {
	re     *regexp.Regexp
	negate bool
}

func sourceMatchers(t *ast.Task) ([]sourceMatcher, error) {
	dir := realPath(t.Dir)
	matchers := make([]sourceMatcher, 0, len(t.Sources))
	for _, g := range t.Sources {
		abs := filepath.ToSlash(filepathext.SmartJoin(dir, g.Glob))
		expr, err := pattern.Regexp(abs, pattern.Filenames|pattern.EntireString)
		if err != nil {
			// Patterns the matcher does not understand (e.g. brace
			// expansion) are skipped for deleted files.
			continue
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			continue
		}
		matchers = append(matchers, sourceMatcher{re: re, negate: g.Negate})
	}
	return matchers, nil
}

// matchesSources applies the patterns in order, so a later negated pattern
// can exclude a file matched earlier, as with fingerprint.Globs.
func matchesSources(matchers []sourceMatcher, path string) bool {
	matched := false
	for _, m := range matchers {
		if m.re.MatchString(path) {
			matched = !m.negate
		}
	}
	return matched
}

// realPath resolves symlinks in path, or in its closest existing parent for
// paths that no longer exist, so paths reported by git and paths built from
// the Taskfile's directory compare equal.
func realPath(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(realPath(parent), filepath.Base(path))
}

func (e *Executor) relativeToRoot(path string) string {
	rel, err := filepath.Rel(realPath(e.Dir), path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// WriteAffected writes the affected tasks as text or JSON. Internal tasks are
// left out, since they cannot be run directly.
func (e *Executor) WriteAffected(w io.Writer, affected []*AffectedTask, asJSON bool) error {
	visible := make([]*AffectedTask, 0, len(affected))
	for _, a := range affected {
		if t, ok := e.Taskfile.Tasks.Get(a.Task); ok && t.Internal {
			continue
		}
		visible = append(visible, a)
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(visible)
	}

	width := 0
	for _, a := range visible {
		width = max(width, len(a.Task))
	}
	var b strings.Builder
	for _, a := range visible {
		var why []string
		if len(a.Files) > 0 {
			const shown = 3
			files := strings.Join(a.Files[:min(shown, len(a.Files))], ", ")
			if len(a.Files) > shown {
				files += fmt.Sprintf(" (+%d more)", len(a.Files)-shown)
			}
			why = append(why, files)
		}
		if a.TaskfileChanged {
			why = append(why, "Taskfile changed")
		}
		if len(a.Via) > 0 {
			why = append(why, "via "+strings.Join(a.Via, ", "))
		}
		fmt.Fprintf(&b, "%-*s  %s\n", width, a.Task, strings.Join(why, "; "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
