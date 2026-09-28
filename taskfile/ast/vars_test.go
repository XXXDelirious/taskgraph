package ast_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

func TestVarsConcurrentSetAndIterate(t *testing.T) {
	t.Parallel()

	vars := ast.NewVars()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 200 {
				vars.Set(fmt.Sprintf("W%d_%d", i, j), ast.Var{Value: j})
			}
		})
		wg.Go(func() {
			for range 200 {
				for range vars.All() {
				}
				_ = vars.ToCacheMap()
				ast.NewVars().Merge(vars, nil)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, 800, vars.Len())
}

func TestVarsSetWhileIterating(t *testing.T) {
	t.Parallel()

	vars := ast.NewVars(
		&ast.VarElement{Key: "A", Value: ast.Var{Value: "a"}},
		&ast.VarElement{Key: "B", Value: ast.Var{Value: "b"}},
	)
	// Changing vars inside a loop over it must not deadlock, and the loop
	// sees the variables as they were when it started.
	var seen []string
	for k := range vars.All() {
		seen = append(seen, k)
		vars.Set(k+"_copy", ast.Var{Value: k})
	}
	assert.Equal(t, []string{"A", "B"}, seen)
	assert.Equal(t, 4, vars.Len())
}
