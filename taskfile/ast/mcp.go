package ast

import (
	"go.yaml.in/yaml/v3"

	"github.com/XXXDelirious/taskgraph/errors"
)

// MCP controls how a task is exposed as a tool by `taskgraph --mcp`. It is a
// taskgraph extension; upstream Task ignores the key.
//
// It can be written as a boolean (`mcp: false` hides the task) or as a map:
//
//	mcp:
//	  expose: true
//	  read_only: true
type MCP struct {
	// Expose overrides whether the task is offered as a tool. By default a
	// task is exposed when it has a description and is not internal.
	Expose *bool
	// ReadOnly hints to clients that the task does not change anything.
	ReadOnly bool
	// Destructive hints to clients that the task may delete or overwrite
	// things, e.g. a deploy or a database reset.
	Destructive bool
	// Idempotent hints that running the task twice has no extra effect.
	Idempotent bool
}

func (m *MCP) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var expose bool
		if err := node.Decode(&expose); err != nil {
			return errors.NewTaskfileDecodeError(err, node)
		}
		m.Expose = &expose
		return nil
	case yaml.MappingNode:
		var v struct {
			Expose      *bool
			ReadOnly    bool `yaml:"read_only"`
			Destructive bool
			Idempotent  bool
		}
		if err := node.Decode(&v); err != nil {
			return errors.NewTaskfileDecodeError(err, node)
		}
		m.Expose = v.Expose
		m.ReadOnly = v.ReadOnly
		m.Destructive = v.Destructive
		m.Idempotent = v.Idempotent
		return nil
	}
	return errors.NewTaskfileDecodeError(nil, node).WithTypeMessage("mcp")
}

func (m *MCP) DeepCopy() *MCP {
	if m == nil {
		return nil
	}
	c := *m
	if m.Expose != nil {
		expose := *m.Expose
		c.Expose = &expose
	}
	return &c
}
