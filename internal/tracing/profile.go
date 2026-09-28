package tracing

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// WriteChromeTrace writes spans in the Chrome Trace Event format, which
// https://ui.perfetto.dev, chrome://tracing and speedscope can open.
//
// Spans are spread over "lanes" (threads in the viewer) so that spans on the
// same lane nest properly: tasks that run in parallel get separate lanes.
func WriteChromeTrace(w io.Writer, spans []*Span) error {
	type event struct {
		Name string            `json:"name"`
		Cat  string            `json:"cat,omitempty"`
		Ph   string            `json:"ph"`
		Ts   int64             `json:"ts"`
		Dur  int64             `json:"dur,omitempty"`
		Pid  int               `json:"pid"`
		Tid  int               `json:"tid"`
		Args map[string]string `json:"args,omitempty"`
	}

	var origin time.Time
	if len(spans) > 0 {
		origin = spans[0].Start
		for _, s := range spans {
			if s.Start.Before(origin) {
				origin = s.Start
			}
		}
	}
	lanes := assignLanes(spans)

	events := make([]event, 0, len(spans)+8)
	maxLane := 0
	for _, s := range spans {
		args := make(map[string]string, len(s.Attrs)+2)
		maps.Copy(args, s.Attrs)
		if s.Status != "" {
			args["status"] = s.Status
		}
		if s.Err != "" {
			args["error"] = s.Err
		}
		lane := lanes[s.ID]
		maxLane = max(maxLane, lane)
		events = append(events, event{
			Name: s.Name,
			Cat:  string(s.Kind),
			Ph:   "X",
			Ts:   s.Start.Sub(origin).Microseconds(),
			Dur:  max(s.End.Sub(s.Start).Microseconds(), 1),
			Pid:  1,
			Tid:  lane,
			Args: args,
		})
	}
	events = append(events, event{Name: "process_name", Ph: "M", Pid: 1, Args: map[string]string{"name": "taskgraph"}})
	for lane := 1; lane <= maxLane; lane++ {
		events = append(events, event{Name: "thread_name", Ph: "M", Pid: 1, Tid: lane, Args: map[string]string{"name": fmt.Sprintf("lane %d", lane)}})
	}

	enc := json.NewEncoder(w)
	return enc.Encode(map[string]any{"traceEvents": events, "displayTimeUnit": "ms"})
}

// assignLanes gives each span a lane (1, 2, ...) such that, on every lane,
// spans either nest or do not overlap. A span goes on its parent's lane when
// it can, so sequential work stays together.
func assignLanes(spans []*Span) map[int]int {
	sorted := slices.Clone(spans)
	slices.SortStableFunc(sorted, func(a, b *Span) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		// Longer spans first, so parents come before children that start
		// at the same instant.
		return b.End.Compare(a.End)
	})

	lanes := map[int]int{}
	// open[lane] is the stack of spans on that lane that may still contain
	// later spans.
	var open [][]*Span
	fits := func(lane int, s *Span) bool {
		stack := open[lane]
		for len(stack) > 0 && !stack[len(stack)-1].End.After(s.Start) {
			stack = stack[:len(stack)-1]
		}
		open[lane] = stack
		if len(stack) == 0 {
			return true
		}
		top := stack[len(stack)-1]
		return isAncestor(top, s) && !s.End.After(top.End)
	}

	for _, s := range sorted {
		lane := -1
		if s.Parent != nil {
			if pl, ok := lanes[s.Parent.ID]; ok && fits(pl-1, s) {
				lane = pl - 1
			}
		}
		for i := 0; lane < 0 && i < len(open); i++ {
			if fits(i, s) {
				lane = i
			}
		}
		if lane < 0 {
			open = append(open, nil)
			lane = len(open) - 1
		}
		open[lane] = append(open[lane], s)
		lanes[s.ID] = lane + 1
	}
	return lanes
}

func isAncestor(a, s *Span) bool {
	for p := s.Parent; p != nil; p = p.Parent {
		if p.ID == a.ID {
			return true
		}
	}
	return false
}

// Summary describes where the time went in a run.
type Summary struct {
	Total time.Duration
	// CriticalPath is the chain of tasks that determined the total time,
	// outermost first: each task's slowest-finishing child task.
	CriticalPath []PathStep
	// Slowest are the tasks with the most self time: time spent in their own
	// commands and checks rather than in the tasks they run.
	Slowest []TaskTime
}

// PathStep is one task on the critical path.
type PathStep struct {
	Task     string
	Duration time.Duration
	Status   string
}

// TaskTime is the time spent in one task.
type TaskTime struct {
	Task     string
	Self     time.Duration
	Duration time.Duration
	Status   string
}

// Summarize works out the critical path and slowest tasks.
func Summarize(spans []*Span, slowest int) *Summary {
	sum := &Summary{}
	children := map[int][]*Span{}
	var roots []*Span
	for _, s := range spans {
		if s.Kind != KindTask {
			continue
		}
		parent := nearestTask(s.Parent)
		if parent == nil {
			roots = append(roots, s)
		} else {
			children[parent.ID] = append(children[parent.ID], s)
		}
	}
	if len(spans) > 0 {
		start, end := spans[0].Start, spans[0].End
		for _, s := range spans {
			if s.Start.Before(start) {
				start = s.Start
			}
			if s.End.After(end) {
				end = s.End
			}
		}
		sum.Total = end.Sub(start)
	}

	// The critical path starts at the root task that finished last, and
	// follows the child task that finished last at each step.
	next := latestEnd(roots)
	for next != nil {
		sum.CriticalPath = append(sum.CriticalPath, PathStep{Task: next.Name, Duration: next.End.Sub(next.Start), Status: next.Status})
		next = latestEnd(children[next.ID])
	}

	var times []TaskTime
	for _, s := range spans {
		if s.Kind != KindTask {
			continue
		}
		times = append(times, TaskTime{
			Task:     s.Name,
			Duration: s.End.Sub(s.Start),
			Self:     s.End.Sub(s.Start) - covered(children[s.ID]),
			Status:   s.Status,
		})
	}
	slices.SortStableFunc(times, func(a, b TaskTime) int { return cmp.Compare(b.Self, a.Self) })
	sum.Slowest = times[:min(slowest, len(times))]
	return sum
}

func nearestTask(s *Span) *Span {
	for ; s != nil; s = s.Parent {
		if s.Kind == KindTask {
			return s
		}
	}
	return nil
}

func latestEnd(spans []*Span) *Span {
	var latest *Span
	for _, s := range spans {
		if latest == nil || s.End.After(latest.End) {
			latest = s
		}
	}
	return latest
}

// covered returns the total time covered by the spans, counting overlapping
// time once.
func covered(spans []*Span) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	sorted := slices.Clone(spans)
	slices.SortFunc(sorted, func(a, b *Span) int { return a.Start.Compare(b.Start) })
	var total time.Duration
	start, end := sorted[0].Start, sorted[0].End
	for _, s := range sorted[1:] {
		if s.Start.After(end) {
			total += end.Sub(start)
			start, end = s.Start, s.End
		} else if s.End.After(end) {
			end = s.End
		}
	}
	return total + end.Sub(start)
}

// WriteSummary writes a human-readable summary.
func (sum *Summary) WriteSummary(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Total: %s\n", formatDuration(sum.Total))
	if len(sum.CriticalPath) > 0 {
		b.WriteString("\nCritical path:\n")
		for i, step := range sum.CriticalPath {
			indent := strings.Repeat("   ", i)
			arrow := ""
			if i > 0 {
				arrow = "└─ "
			}
			fmt.Fprintf(&b, "  %s%s%s  %s%s\n", indent, arrow, step.Task, formatDuration(step.Duration), statusNote(step.Status))
		}
	}
	if len(sum.Slowest) > 0 {
		b.WriteString("\nSlowest tasks (own time, excluding tasks they run):\n")
		width := 0
		for _, t := range sum.Slowest {
			width = max(width, len(t.Task))
		}
		for _, t := range sum.Slowest {
			fmt.Fprintf(&b, "  %-*s  %8s%s\n", width, t.Task, formatDuration(t.Self), statusNote(t.Status))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func statusNote(status string) string {
	switch status {
	case "", StatusRan:
		return ""
	default:
		return " (" + status + ")"
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}
