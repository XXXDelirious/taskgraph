package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/args"
	"github.com/XXXDelirious/taskgraph/errors"
	"github.com/XXXDelirious/taskgraph/experiments"
	"github.com/XXXDelirious/taskgraph/internal/filepathext"
	"github.com/XXXDelirious/taskgraph/internal/flags"
	"github.com/XXXDelirious/taskgraph/internal/logger"
	"github.com/XXXDelirious/taskgraph/internal/version"
	"github.com/XXXDelirious/taskgraph/mcpserver"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

func main() {
	if err := run(); err != nil {
		l := &logger.Logger{
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			Verbose: flags.Verbose,
			Color:   flags.Color,
		}
		if err, ok := err.(*errors.TaskRunError); ok && flags.ExitCode {
			emitCIErrorAnnotation(err)
			l.Errf(logger.Red, "%v\n", err)
			os.Exit(err.TaskExitCode())
		}
		if err, ok := err.(errors.TaskError); ok {
			emitCIErrorAnnotation(err)
			l.Errf(logger.Red, "%v\n", err)
			os.Exit(err.Code())
		}
		emitCIErrorAnnotation(err)
		l.Errf(logger.Red, "%v\n", err)
		os.Exit(errors.CodeUnknown)
	}
	os.Exit(errors.CodeOk)
}

// emitCIErrorAnnotation emits an error annotation for supported CI providers.
func emitCIErrorAnnotation(err error) {
	if isGA, _ := strconv.ParseBool(os.Getenv("GITHUB_ACTIONS")); !isGA {
		return
	}
	if e, ok := err.(*errors.TaskRunError); ok {
		fmt.Fprintf(os.Stdout, "::error title=Task '%s' failed::%v\n", e.TaskName, e.Err)
		return
	}
	fmt.Fprintf(os.Stdout, "::error title=Task failed::%v\n", err)
}

func run() error {
	log := &logger.Logger{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Verbose: flags.Verbose,
		Color:   flags.Color,
	}

	if err := flags.Validate(); err != nil {
		return err
	}

	if err := experiments.Validate(); err != nil {
		log.Warnf("%s\n", err.Error())
	}

	if flags.Version {
		fmt.Printf("taskgraph %s (compatible with Task %s)\n", version.GetVersionWithBuildInfo(), version.GetTaskCompatVersion())
		return nil
	}

	if flags.Help {
		pflag.Usage()
		return nil
	}

	if flags.Experiments {
		return log.PrintExperiments()
	}

	if flags.Init {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		args, _, err := args.Get()
		if err != nil {
			return err
		}
		path := wd
		if len(args) > 0 {
			name := args[0]
			if filepathext.IsExtOnly(name) {
				name = filepathext.SmartJoin(filepath.Dir(name), "Taskfile"+filepath.Ext(name))
			}
			path = filepathext.SmartJoin(wd, name)
		}
		finalPath, err := task.InitTaskfile(path)
		if err != nil {
			return err
		}
		if !flags.Silent {
			if flags.Verbose {
				log.Outf(logger.Default, "%s\n", task.DefaultTaskfile)
			}
			log.Outf(logger.Green, "Taskfile created: %s\n", filepathext.TryAbsToRel(finalPath))
		}
		return nil
	}

	if flags.Completion != "" {
		script, err := task.Completion(flags.Completion)
		if err != nil {
			return err
		}
		fmt.Println(script)
		return nil
	}

	if flags.MCP {
		return runMCPServer()
	}

	e := task.NewExecutor(
		flags.WithFlags(),
		task.WithVersionCheck(true),
	)
	if err := e.Setup(); err != nil {
		return err
	}

	if flags.ClearCache {
		cachePath := filepath.Join(e.TempDir.Remote, "remote")
		return os.RemoveAll(cachePath)
	}

	listOptions := task.NewListOptions(
		flags.List,
		flags.ListAll,
		flags.ListJson,
		flags.NoStatus,
		flags.Nested,
	)
	if listOptions.ShouldListTasks() {
		if flags.Silent {
			return e.ListTaskNames(flags.ListAll)
		}
		foundTasks, err := e.ListTasks(listOptions)
		if err != nil {
			return err
		}
		if !foundTasks {
			os.Exit(errors.CodeUnknown)
		}
		return nil
	}

	// Parse the remaining arguments
	cliArgsPreDash, cliArgsPostDash, err := args.Get()
	if err != nil {
		return err
	}
	calls, globals := args.Parse(cliArgsPreDash...)
	requestedCalls := slices.Clone(calls)

	// If there are no calls, run the default task instead
	if len(calls) == 0 {
		calls = append(calls, &task.Call{Task: "default"})
	}

	// Merge CLI variables first (e.g. FOO=bar) so they take priority over Taskfile defaults
	e.Taskfile.Vars.Merge(globals, nil)

	// Then ReverseMerge special variables so they're available for templating
	cliArgsPostDashQuoted, err := args.ToQuotedString(cliArgsPostDash)
	if err != nil {
		return err
	}
	specialVars := ast.NewVars()
	specialVars.Set("CLI_ARGS", ast.Var{Value: cliArgsPostDashQuoted})
	specialVars.Set("CLI_ARGS_LIST", ast.Var{Value: cliArgsPostDash})
	specialVars.Set("CLI_FORCE", ast.Var{Value: flags.Force || flags.ForceAll})
	specialVars.Set("CLI_SILENT", ast.Var{Value: flags.Silent})
	specialVars.Set("CLI_VERBOSE", ast.Var{Value: flags.Verbose})
	specialVars.Set("CLI_OFFLINE", ast.Var{Value: flags.Offline})
	specialVars.Set("CLI_ASSUME_YES", ast.Var{Value: flags.AssumeYes})
	e.Taskfile.Vars.ReverseMerge(specialVars, nil)
	if !flags.Watch {
		e.InterceptInterruptSignals()
	}

	ctx := context.Background()

	if flags.Graph {
		// With no task names, graph the whole Taskfile.
		g, err := e.TaskGraph(requestedCalls...)
		if err != nil {
			return err
		}
		return g.Write(os.Stdout, flags.GraphFormat)
	}

	if flags.Explain {
		return e.Explain(ctx, os.Stdout, flags.ListJson, calls...)
	}

	if flags.Affected {
		changed, err := e.ChangedFiles(ctx, flags.Since)
		if err != nil {
			return err
		}
		// With no task names, list what is affected instead of running it.
		if len(requestedCalls) == 0 {
			affected, err := e.AffectedTasks(changed)
			if err != nil {
				return err
			}
			return e.WriteAffected(os.Stdout, affected, flags.ListJson)
		}
		calls, err = affectedCalls(e, changed, calls)
		if err != nil {
			return err
		}
		if len(calls) == 0 {
			if !flags.Silent {
				log.Outf(logger.Green, "task: No tasks are affected by the changes\n")
			}
			return nil
		}
	}

	if flags.Status {
		return e.Status(ctx, calls...)
	}

	return e.Run(ctx, calls...)
}

// runMCPServer serves the Taskfile over MCP on stdin/stdout. Nothing else may
// be written to stdout while it runs, since stdout carries the protocol.
func runMCPServer() error {
	cliArgs, _, err := args.Get()
	if err != nil {
		return err
	}
	calls, _ := args.Parse(cliArgs...)
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Task)
	}

	server, err := mcpserver.New(mcpserver.Options{
		ExecutorOptions: []task.ExecutorOption{flags.WithFlags(), task.WithVersionCheck(true)},
		Tasks:           names,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if flags.Verbose {
		fmt.Fprintf(os.Stderr, "task: serving %d tools over MCP: %s\n", len(server.Tools()), strings.Join(server.Tools(), ", "))
	}
	return server.Run(ctx)
}

// affectedCalls returns the calls whose task is affected by the changed
// files, and reports the others as skipped.
func affectedCalls(e *task.Executor, changed []string, calls []*task.Call) ([]*task.Call, error) {
	var affected []*task.Call
	for _, c := range calls {
		a, err := e.AffectedCall(changed, c)
		if err != nil {
			return nil, err
		}
		if a != nil {
			affected = append(affected, c)
			continue
		}
		if !flags.Silent {
			e.Logger.Errf(logger.Yellow, "task: %q is not affected by the changes - skipped\n", c.Task)
		}
	}
	return affected, nil
}
