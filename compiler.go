package task

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/XXXDelirious/taskgraph/internal/env"
	"github.com/XXXDelirious/taskgraph/internal/execext"
	"github.com/XXXDelirious/taskgraph/internal/filepathext"
	"github.com/XXXDelirious/taskgraph/internal/logger"
	"github.com/XXXDelirious/taskgraph/internal/templater"
	"github.com/XXXDelirious/taskgraph/internal/version"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

type Compiler struct {
	Dir            string
	Entrypoint     string
	UserWorkingDir string

	TaskfileEnv  *ast.Vars
	TaskfileVars *ast.Vars

	Logger *logger.Logger

	dynamicCache   map[dynamicCacheKey]*dynamicResult
	muDynamicCache sync.Mutex
}

// dynamicCacheKey identifies the result of a dynamic variable. The directory
// is part of it: the same command can print different things in different
// directories (e.g. `pwd` or `git rev-parse --show-prefix`).
type dynamicCacheKey struct {
	sh  string
	dir string
}

// dynamicResult is the result of running a dynamic variable's command. done
// is closed once value and err are set, so concurrent callers asking for the
// same variable wait for one run instead of starting their own.
type dynamicResult struct {
	done  chan struct{}
	value string
	err   error
}

func (c *Compiler) GetTaskfileVariables() (*ast.Vars, error) {
	return c.getVariables(nil, nil, true)
}

func (c *Compiler) GetVariables(t *ast.Task, call *Call) (*ast.Vars, error) {
	return c.getVariables(t, call, true)
}

func (c *Compiler) FastGetVariables(t *ast.Task, call *Call) (*ast.Vars, error) {
	return c.getVariables(t, call, false)
}

func (c *Compiler) getVariables(t *ast.Task, call *Call, evaluateShVars bool) (*ast.Vars, error) {
	result := env.GetEnviron()
	specialVars, err := c.getSpecialVars(t, call)
	if err != nil {
		return nil, err
	}
	for k, v := range specialVars {
		result.Set(k, ast.Var{Value: v})
	}

	getRangeFunc := func(dir string) func(k string, v ast.Var) error {
		return func(k string, v ast.Var) error {
			cache := &templater.Cache{Vars: result}
			// Replace values
			newVar := templater.ReplaceVar(v, cache)
			// If the variable should not be evaluated, but is nil, set it to an empty string
			// This stops empty interface errors when using the templater to replace values later
			// Preserve the Sh field so it can be displayed in summary
			if !evaluateShVars && newVar.Value == nil {
				result.Set(k, ast.Var{Value: "", Sh: newVar.Sh})
				return nil
			}
			// If the variable should not be evaluated and it is set, we can set it and return
			if !evaluateShVars {
				result.Set(k, ast.Var{Value: newVar.Value, Sh: newVar.Sh})
				return nil
			}
			// Now we can check for errors since we've handled all the cases when we don't want to evaluate
			if err := cache.Err(); err != nil {
				return err
			}
			// If the variable is already set, we can set it and return
			if newVar.Value != nil || newVar.Sh == nil {
				result.Set(k, ast.Var{Value: newVar.Value})
				return nil
			}
			// If the variable is dynamic, we need to resolve it first
			static, err := c.HandleDynamicVar(newVar, dir, env.GetFromVars(result))
			if err != nil {
				return err
			}
			result.Set(k, ast.Var{Value: static})
			return nil
		}
	}
	rangeFunc := getRangeFunc(c.Dir)

	// taskRangeFunc evaluates the task's own variables in the task's dir.
	// The dir may be a template that uses variables, so it is worked out
	// again once more variables are known (see setTaskDir below).
	var taskRangeFunc func(k string, v ast.Var) error
	setTaskDir := func() error {
		// NOTE(@andreynering): We're manually joining these paths here because
		// this is the raw task, not the compiled one.
		cache := &templater.Cache{Vars: result}
		dir := templater.Replace(t.Dir, cache)
		if err := cache.Err(); err != nil {
			return err
		}
		taskRangeFunc = getRangeFunc(filepathext.SmartJoin(c.Dir, dir))
		return nil
	}

	for k, v := range c.TaskfileEnv.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	for k, v := range c.TaskfileVars.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	if t != nil {
		for k, v := range t.IncludeVars.All() {
			if err := rangeFunc(k, v); err != nil {
				return nil, err
			}
		}
		if err := setTaskDir(); err != nil {
			return nil, err
		}
		for k, v := range t.IncludedTaskfileVars.All() {
			if err := taskRangeFunc(k, v); err != nil {
				return nil, err
			}
		}
	}

	if t == nil || call == nil {
		return result, nil
	}

	for k, v := range call.Vars.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	if err := setTaskDir(); err != nil {
		return nil, err
	}
	for k, v := range t.Vars.All() {
		if err := taskRangeFunc(k, v); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (c *Compiler) HandleDynamicVar(v ast.Var, dir string, e []string) (string, error) {
	// If the variable is not dynamic or it is empty, return an empty string
	if v.Sh == nil || *v.Sh == "" {
		return "", nil
	}

	// NOTE(@andreynering): If a var have a specific dir, use this instead
	if v.Dir != "" {
		dir = v.Dir
	}
	key := dynamicCacheKey{sh: *v.Sh, dir: dir}

	// The lock only guards the map, so dynamic variables of tasks running in
	// parallel are evaluated in parallel too.
	c.muDynamicCache.Lock()
	if c.dynamicCache == nil {
		c.dynamicCache = make(map[dynamicCacheKey]*dynamicResult, 30)
	}
	if r, ok := c.dynamicCache[key]; ok {
		c.muDynamicCache.Unlock()
		<-r.done
		return r.value, r.err
	}
	r := &dynamicResult{done: make(chan struct{})}
	c.dynamicCache[key] = r
	c.muDynamicCache.Unlock()

	r.value, r.err = c.runDynamicVar(*v.Sh, dir, e)
	close(r.done)
	if r.err != nil {
		// Failures are not cached, so a later call can try again.
		c.muDynamicCache.Lock()
		if c.dynamicCache[key] == r {
			delete(c.dynamicCache, key)
		}
		c.muDynamicCache.Unlock()
	}
	return r.value, r.err
}

func (c *Compiler) runDynamicVar(sh, dir string, e []string) (string, error) {
	var stdout bytes.Buffer
	opts := &execext.RunCommandOptions{
		Command: sh,
		Dir:     dir,
		Stdout:  &stdout,
		Stderr:  c.Logger.Stderr,
		Env:     e,
	}
	if err := execext.RunCommand(context.Background(), opts); err != nil {
		return "", fmt.Errorf(`task: Command "%s" failed: %s`, opts.Command, err)
	}

	// Trim a single trailing newline from the result to make most command
	// output easier to use in shell commands.
	result := strings.TrimSuffix(stdout.String(), "\r\n")
	result = strings.TrimSuffix(result, "\n")

	c.Logger.VerboseErrf(logger.Magenta, "task: dynamic variable: %q result: %q\n", sh, result)
	return result, nil
}

// ResetCache clear the dynamic variables cache
func (c *Compiler) ResetCache() {
	c.muDynamicCache.Lock()
	defer c.muDynamicCache.Unlock()

	c.dynamicCache = nil
}

func (c *Compiler) getSpecialVars(t *ast.Task, call *Call) (map[string]string, error) {
	// Use filepath.ToSlash for all paths to ensure consistent forward slashes
	// across platforms. This prevents issues with backslashes being interpreted
	// as escape sequences when paths are used in shell commands on Windows.
	allVars := map[string]string{
		"TASK_EXE":            filepath.ToSlash(os.Args[0]),
		"ROOT_TASKFILE":       filepath.ToSlash(filepathext.SmartJoin(c.Dir, c.Entrypoint)),
		"ROOT_DIR":            filepath.ToSlash(c.Dir),
		"USER_WORKING_DIR":    filepath.ToSlash(c.UserWorkingDir),
		"TASK_VERSION":        version.GetTaskCompatVersion(),
		"TASKGRAPH_VERSION":   version.GetVersion(),
		"PATH_LIST_SEPARATOR": string(os.PathListSeparator),
		"FILE_PATH_SEPARATOR": string(os.PathSeparator),
	}
	if t != nil {
		allVars["TASK"] = t.Task
		allVars["TASK_DIR"] = filepath.ToSlash(filepathext.SmartJoin(c.Dir, t.Dir))
		allVars["TASKFILE"] = filepath.ToSlash(t.Location.Taskfile)
		allVars["TASKFILE_DIR"] = filepath.ToSlash(filepath.Dir(t.Location.Taskfile))
	} else {
		allVars["TASK"] = ""
		allVars["TASK_DIR"] = ""
		allVars["TASKFILE"] = ""
		allVars["TASKFILE_DIR"] = ""
	}
	if call != nil {
		allVars["ALIAS"] = call.Task
	} else {
		allVars["ALIAS"] = ""
	}

	return allVars, nil
}
