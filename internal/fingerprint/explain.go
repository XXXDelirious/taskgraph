package fingerprint

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/XXXDelirious/taskgraph/internal/env"
	"github.com/XXXDelirious/taskgraph/internal/execext"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// Kinds of [FileChange].
const (
	ChangeAdded    = "added"
	ChangeModified = "modified"
	ChangeRemoved  = "removed"
	// ChangeNewer is used by the timestamp method: the source file is newer
	// than the task's generated files.
	ChangeNewer = "newer"
)

// FileChange is a source file that makes a task out of date.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// Explanation says whether a task is up to date, and why.
type Explanation struct {
	UpToDate bool `json:"upToDate"`
	// Method is the fingerprinting method used for the task's sources. It is
	// empty when the task has no sources.
	Method string `json:"method,omitempty"`
	// Reasons are short, human-readable sentences.
	Reasons []string `json:"reasons"`
	// SourceFiles is the number of files matched by the task's sources.
	SourceFiles int `json:"sourceFiles"`
	// Changes lists the source files that make the task out of date, when
	// they can be determined.
	Changes []FileChange `json:"changes,omitempty"`
	// MissingGenerates lists `generates` patterns that match no file.
	MissingGenerates []string `json:"missingGenerates,omitempty"`
	// FailedStatus lists `status` commands that exited non-zero.
	FailedStatus []string `json:"failedStatus,omitempty"`
}

// Explain works out whether a task is up to date, like [IsTaskUpToDate], and
// records why. Unlike IsTaskUpToDate it never writes to the temp dir. It does
// run the task's `status` commands.
func Explain(ctx context.Context, t *ast.Task, method, tempDir string) (*Explanation, error) {
	ex := &Explanation{}
	statusIsSet := len(t.Status) != 0
	sourcesIsSet := len(t.Sources) != 0

	if !statusIsSet && !sourcesIsSet {
		ex.Reasons = append(ex.Reasons, "it has no sources or status checks, so it always runs")
		return ex, nil
	}

	statusUpToDate := true
	if statusIsSet {
		for _, s := range t.Status {
			err := execext.RunCommand(ctx, &execext.RunCommandOptions{
				Command: s,
				Dir:     t.Dir,
				Env:     env.Get(t),
			})
			if err != nil {
				ex.FailedStatus = append(ex.FailedStatus, s)
			}
		}
		statusUpToDate = len(ex.FailedStatus) == 0
		if statusUpToDate {
			ex.Reasons = append(ex.Reasons, "all status checks passed")
		} else {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("%d of %d status checks failed", len(ex.FailedStatus), len(t.Status)))
		}
	}

	sourcesUpToDate := true
	if sourcesIsSet {
		ex.Method = method
		var err error
		switch method {
		case "checksum":
			sourcesUpToDate, err = explainChecksum(t, tempDir, ex)
		case "timestamp":
			sourcesUpToDate, err = explainTimestamp(t, tempDir, ex)
		case "none":
			sourcesUpToDate = false
			ex.Reasons = append(ex.Reasons, `method is "none", so sources never count as up to date`)
		default:
			return nil, fmt.Errorf(`task: invalid method "%s"`, method)
		}
		if err != nil {
			return nil, err
		}
	}

	ex.UpToDate = statusUpToDate && sourcesUpToDate
	return ex, nil
}

func explainChecksum(t *ast.Task, tempDir string, ex *Explanation) (bool, error) {
	checker := NewChecksumChecker(tempDir, true)
	newHash, files, err := checker.checksumFiles(t)
	if err != nil {
		return false, err
	}
	ex.SourceFiles = len(files)

	upToDate := true
	data, _ := os.ReadFile(checker.checksumFilePath(t))
	oldHash := strings.TrimSpace(string(data))
	switch oldHash {
	case "":
		upToDate = false
		ex.Reasons = append(ex.Reasons, "no previous successful run is recorded")
	case newHash:
		ex.Reasons = append(ex.Reasons, fmt.Sprintf("%s unchanged since the last run", pluralFiles(len(files))))
	default:
		upToDate = false
		previous, err := readManifest(checker.manifestFilePath(t))
		if err != nil {
			return false, err
		}
		if previous == nil {
			ex.Reasons = append(ex.Reasons, "source files changed since the last run (the previous run did not record which ones)")
			break
		}
		ex.Changes = diffManifests(previous, files)
		if len(ex.Changes) == 0 {
			// Same contents under different names, e.g. a rename.
			ex.Reasons = append(ex.Reasons, "source files were renamed since the last run")
		} else {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("%s changed since the last run", pluralFiles(len(ex.Changes))))
		}
	}

	if !explainGenerates(t, ex) {
		upToDate = false
	}
	return upToDate, nil
}

func explainTimestamp(t *ast.Task, tempDir string, ex *Explanation) (bool, error) {
	checker := NewTimestampChecker(tempDir, true)
	sources, err := Globs(t.Dir, t.Sources)
	if err != nil {
		return false, err
	}
	ex.SourceFiles = len(sources)

	if !explainGenerates(t, ex) {
		return false, nil
	}

	generates, err := Globs(t.Dir, t.Generates)
	if err != nil {
		return false, err
	}
	if timestampFile := checker.timestampFilePath(t); fileExists(timestampFile) {
		generates = append(generates, timestampFile)
	}
	generateMaxTime, err := getMaxTime(generates...)
	if err != nil {
		return false, err
	}
	if generateMaxTime.IsZero() {
		ex.Reasons = append(ex.Reasons, "no previous run is recorded and there are no generated files to compare with")
		return false, nil
	}

	for _, f := range sources {
		info, err := os.Stat(f)
		if err != nil {
			return false, err
		}
		if info.ModTime().After(generateMaxTime) {
			ex.Changes = append(ex.Changes, FileChange{Path: relativeTo(t.Dir, f), Change: ChangeNewer})
		}
	}
	if len(ex.Changes) > 0 {
		ex.Reasons = append(ex.Reasons, fmt.Sprintf("%s newer than the last run", pluralFiles(len(ex.Changes))))
		return false, nil
	}
	ex.Reasons = append(ex.Reasons, fmt.Sprintf("%s older than the last run", pluralFiles(len(sources))))
	return true, nil
}

// explainGenerates records `generates` patterns that match no file, and
// returns false if there are any.
func explainGenerates(t *ast.Task, ex *Explanation) bool {
	for _, g := range t.Generates {
		if g.Negate {
			continue
		}
		files, err := glob(t.Dir, g.Glob)
		if err != nil || len(files) == 0 {
			ex.MissingGenerates = append(ex.MissingGenerates, g.Glob)
		}
	}
	if len(ex.MissingGenerates) > 0 {
		if len(ex.MissingGenerates) == 1 {
			ex.Reasons = append(ex.Reasons, "1 generated file pattern matches nothing")
		} else {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("%d generated file patterns match nothing", len(ex.MissingGenerates)))
		}
		return false
	}
	return true
}

func diffManifests(previous, current map[string]string) []FileChange {
	var changes []FileChange
	for path, hash := range current {
		old, ok := previous[path]
		switch {
		case !ok:
			changes = append(changes, FileChange{Path: path, Change: ChangeAdded})
		case old != hash:
			changes = append(changes, FileChange{Path: path, Change: ChangeModified})
		}
	}
	for path := range previous {
		if _, ok := current[path]; !ok {
			changes = append(changes, FileChange{Path: path, Change: ChangeRemoved})
		}
	}
	slices.SortFunc(changes, func(a, b FileChange) int {
		return strings.Compare(a.Path, b.Path)
	})
	return changes
}

func pluralFiles(n int) string {
	if n == 1 {
		return "1 source file"
	}
	return fmt.Sprintf("%d source files", n)
}
