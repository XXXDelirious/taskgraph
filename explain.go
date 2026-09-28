package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/XXXDelirious/taskgraph/errors"
	"github.com/XXXDelirious/taskgraph/internal/env"
	"github.com/XXXDelirious/taskgraph/internal/execext"
	"github.com/XXXDelirious/taskgraph/internal/fingerprint"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// Verdicts of a [TaskExplanation].
const (
	VerdictWillRun  = "will-run"
	VerdictUpToDate = "up-to-date"
	VerdictSkipped  = "skipped"
	VerdictWillFail = "will-fail"
)

// TaskExplanation says whether a task would run, and why.
type TaskExplanation struct {
	Task string `json:"task"`
	// Kind is how the parent task runs this one: [EdgeDep] or [EdgeCall].
	// It is empty for the tasks given on the command line.
	Kind string `json:"kind,omitempty"`
	// Verdict is one of [VerdictWillRun], [VerdictUpToDate], [VerdictSkipped]
	// or [VerdictWillFail].
	Verdict string `json:"verdict"`
	// Reasons are short, human-readable sentences.
	Reasons []string `json:"reasons"`
	// Fingerprint has the details of the up-to-date check, when one was made.
	Fingerprint *fingerprint.Explanation `json:"fingerprint,omitempty"`
	// SeeAbove is set when this task, with the same variables, was already
	// explained earlier in the output.
	SeeAbove bool `json:"seeAbove,omitempty"`
	// Children are the tasks this one runs: all its deps, plus the tasks it
	// calls from its commands if it will run.
	Children []*TaskExplanation `json:"children,omitempty"`
}

// ExplainTasks works out whether each call, and every task it runs, would run
// and why. It evaluates dynamic variables, `if` conditions, preconditions and
// status checks, the same way a real run would, but it runs no task commands
// and does not update any fingerprint state.
func (e *Executor) ExplainTasks(ctx context.Context, calls ...*Call) ([]*TaskExplanation, error) {
	seen := map[string]*TaskExplanation{}
	result := make([]*TaskExplanation, 0, len(calls))
	for _, c := range calls {
		ex, err := e.explainCall(ctx, c, "", nil, seen)
		if err != nil {
			return nil, err
		}
		result = append(result, ex)
	}
	return result, nil
}

// Explain writes the explanation for the given calls to w, as text or JSON.
func (e *Executor) Explain(ctx context.Context, w io.Writer, asJSON bool, calls ...*Call) error {
	explanations, err := e.ExplainTasks(ctx, calls...)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(explanations)
	}
	var b strings.Builder
	for _, ex := range explanations {
		writeExplanation(&b, ex, "", "")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func (e *Executor) explainCall(ctx context.Context, call *Call, kind string, parents []callFrame, seen map[string]*TaskExplanation) (*TaskExplanation, error) {
	ex := &TaskExplanation{Task: call.Task, Kind: kind}

	fast, err := e.FastCompiledTask(call)
	if err != nil {
		return nil, err
	}
	self := newCallFrame(fast, call)
	if cycle := findCycle(parents, self); cycle != nil {
		return nil, &errors.TaskCycleError{Cycle: cycle}
	}
	if earlier, ok := seen[self.key]; ok {
		ex.SeeAbove = true
		ex.Verdict = earlier.Verdict
		ex.Reasons = []string{"explained above"}
		return ex, nil
	}
	seen[self.key] = ex
	parents = append(parents, self)

	if !shouldRunOnCurrentPlatform(fast.Platforms) {
		ex.Verdict = VerdictSkipped
		ex.Reasons = []string{fmt.Sprintf("not meant for this platform (%s/%s)", runtime.GOOS, runtime.GOARCH)}
		return ex, nil
	}

	t, err := e.CompiledTask(call)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(t.If) != "" && !e.explainCommandSucceeds(ctx, t, t.If) {
		ex.Verdict = VerdictSkipped
		ex.Reasons = []string{fmt.Sprintf("`if` condition is false: %s", t.If)}
		return ex, nil
	}

	if missing := getMissingRequiredVars(t); len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, v := range missing {
			names = append(names, v.Name)
		}
		ex.Verdict = VerdictWillFail
		ex.Reasons = []string{"required variables are not set: " + strings.Join(names, ", ")}
		return ex, nil
	}

	// Deps run before the task's own up-to-date check, so they are explained
	// whatever the verdict for this task is.
	for _, d := range t.Deps {
		if d.Task == "" {
			continue
		}
		child, err := e.explainCall(ctx, &Call{Task: d.Task, Vars: d.Vars, Silent: d.Silent, Indirect: true}, EdgeDep, parents, seen)
		if err != nil {
			return nil, err
		}
		ex.Children = append(ex.Children, child)
	}

	e.explainFingerprint(ctx, call, t, ex)

	// Tasks called from cmds only run when this task's commands run.
	if ex.Verdict == VerdictWillRun {
		for _, c := range t.Cmds {
			if c.Task == "" {
				continue
			}
			child, err := e.explainCall(ctx, &Call{Task: c.Task, Vars: c.Vars, Silent: c.Silent, Indirect: true}, EdgeCall, parents, seen)
			if err != nil {
				return nil, err
			}
			ex.Children = append(ex.Children, child)
		}
	}
	return ex, nil
}

// explainFingerprint sets the verdict from preconditions, --force and the
// task's up-to-date check.
func (e *Executor) explainFingerprint(ctx context.Context, call *Call, t *ast.Task, ex *TaskExplanation) {
	for _, p := range t.Preconditions {
		if !e.explainCommandSucceeds(ctx, t, p.Sh) {
			ex.Verdict = VerdictWillFail
			ex.Reasons = []string{"precondition not met: " + p.Msg}
			return
		}
	}

	if e.ForceAll || (!call.Indirect && e.Force) {
		ex.Verdict = VerdictWillRun
		ex.Reasons = []string{"forced with --force"}
		return
	}

	method := e.Taskfile.Method
	if t.Method != "" {
		method = t.Method
	}
	fp, err := fingerprint.Explain(ctx, t, method, e.TempDir.Fingerprint)
	if err != nil {
		ex.Verdict = VerdictWillFail
		ex.Reasons = []string{"cannot check whether it is up to date: " + err.Error()}
		return
	}
	ex.Fingerprint = fp
	ex.Reasons = fp.Reasons
	if fp.UpToDate {
		ex.Verdict = VerdictUpToDate
	} else {
		ex.Verdict = VerdictWillRun
	}
}

func (e *Executor) explainCommandSucceeds(ctx context.Context, t *ast.Task, command string) bool {
	return execext.RunCommand(ctx, &execext.RunCommandOptions{
		Command: command,
		Dir:     t.Dir,
		Env:     env.Get(t),
	}) == nil
}

var verdictLabels = map[string]string{
	VerdictWillRun:  "will run",
	VerdictUpToDate: "up to date",
	VerdictSkipped:  "skipped",
	VerdictWillFail: "will fail",
}

func writeExplanation(b *strings.Builder, ex *TaskExplanation, prefix, childPrefix string) {
	name := ex.Task
	if ex.Kind == EdgeCall {
		name += " [call]"
	}
	fmt.Fprintf(b, "%s%s: %s", prefix, name, verdictLabels[ex.Verdict])
	if ex.SeeAbove {
		b.WriteString(" (see above)\n")
		return
	}
	b.WriteString("\n")

	detail := childPrefix + "│ "
	if len(ex.Children) == 0 {
		detail = childPrefix + "  "
	}
	for _, r := range ex.Reasons {
		fmt.Fprintf(b, "%s  - %s\n", detail, r)
	}
	if fp := ex.Fingerprint; fp != nil {
		for _, c := range fp.Changes {
			fmt.Fprintf(b, "%s      %-8s %s\n", detail, c.Change, c.Path)
		}
		for _, g := range fp.MissingGenerates {
			fmt.Fprintf(b, "%s      missing  %s\n", detail, g)
		}
		for _, s := range fp.FailedStatus {
			fmt.Fprintf(b, "%s      failed   %s\n", detail, s)
		}
	}

	for i, child := range ex.Children {
		if i == len(ex.Children)-1 {
			writeExplanation(b, child, childPrefix+"└── ", childPrefix+"    ")
		} else {
			writeExplanation(b, child, childPrefix+"├── ", childPrefix+"│   ")
		}
	}
}
