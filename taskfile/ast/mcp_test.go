package ast_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

func TestMCPUnmarshal(t *testing.T) {
	t.Parallel()

	var hidden ast.Task
	require.NoError(t, yaml.Unmarshal([]byte("cmds: [echo]\nmcp: false\n"), &hidden))
	require.NotNil(t, hidden.MCP)
	require.NotNil(t, hidden.MCP.Expose)
	assert.False(t, *hidden.MCP.Expose)

	var hinted ast.Task
	require.NoError(t, yaml.Unmarshal([]byte("cmds: [echo]\nmcp:\n  read_only: true\n  idempotent: true\n"), &hinted))
	require.NotNil(t, hinted.MCP)
	assert.Nil(t, hinted.MCP.Expose)
	assert.True(t, hinted.MCP.ReadOnly)
	assert.True(t, hinted.MCP.Idempotent)
	assert.False(t, hinted.MCP.Destructive)

	copied := hinted.DeepCopy()
	copied.MCP.ReadOnly = false
	assert.True(t, hinted.MCP.ReadOnly, "DeepCopy must not share the MCP settings")

	var bad ast.Task
	assert.Error(t, yaml.Unmarshal([]byte("cmds: [echo]\nmcp: [1, 2]\n"), &bad))
}
