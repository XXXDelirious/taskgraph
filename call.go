package task

import (
	"fmt"
	"strings"

	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// Call is the parameters to a task call
type Call struct {
	Task     string
	Vars     *ast.Vars
	Silent   bool
	Indirect bool // True if the task was called by another task

	// parents is the chain of task calls that led to this call, outermost
	// first. It is used to detect cycles.
	parents []callFrame
}

// callFrame identifies one task call in a call chain.
type callFrame struct {
	// key is the task's canonical name plus its call variables. Calling the
	// same task with the same variables inside itself can never finish.
	key string
	// name is the name the task was called with, for error messages.
	name string
}

// child returns the call chain for a task called by this one.
func (c *Call) child(self callFrame) []callFrame {
	parents := make([]callFrame, 0, len(c.parents)+1)
	parents = append(parents, c.parents...)
	return append(parents, self)
}

// newCallFrame builds the frame for a call to the task t.
func newCallFrame(t *ast.Task, call *Call) callFrame {
	var b strings.Builder
	b.WriteString(t.Task)
	for k, v := range call.Vars.All() {
		// MATCH is derived from the task name, which is already part of the
		// key for wildcard tasks through the call name below.
		if k == "MATCH" {
			continue
		}
		fmt.Fprintf(&b, "\x00%s=%v", k, v.Value)
		// Dynamic and referenced variables may not be resolved yet, so the
		// definition is part of the key too.
		if v.Sh != nil {
			fmt.Fprintf(&b, "\x00sh:%s\x00dir:%s", *v.Sh, v.Dir)
		}
		if v.Ref != "" {
			fmt.Fprintf(&b, "\x00ref:%s", v.Ref)
		}
	}
	if strings.Contains(t.Task, "*") {
		fmt.Fprintf(&b, "\x00call=%s", call.Task)
	}
	return callFrame{key: b.String(), name: call.Task}
}

// findCycle returns the names of the calls that form a cycle ending in self,
// or nil if self does not appear among parents.
func findCycle(parents []callFrame, self callFrame) []string {
	for i, p := range parents {
		if p.key != self.key {
			continue
		}
		cycle := make([]string, 0, len(parents)-i+1)
		for _, f := range parents[i:] {
			cycle = append(cycle, f.name)
		}
		return append(cycle, self.name)
	}
	return nil
}
