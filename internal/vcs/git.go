// Package vcs finds which files changed in a version-controlled project.
package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// ChangedFiles returns the absolute paths of the files that changed in the git
// repository containing dir.
//
// With since empty, it returns uncommitted changes: files that differ from
// HEAD (staged or not) and untracked files that are not ignored. With since
// set to a branch, tag or commit, it also includes every change committed
// since the merge base of since and HEAD, which is what a pull request
// changes.
//
// Deleted and renamed files are included: a rename counts as a deletion of
// the old path plus an addition of the new one.
func ChangedFiles(ctx context.Context, dir, since string) ([]string, error) {
	root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root = strings.TrimSpace(root)

	base := "HEAD"
	if since != "" {
		out, err := git(ctx, dir, "merge-base", since, "HEAD")
		if err != nil {
			return nil, fmt.Errorf("task: cannot compare with %q: %w", since, err)
		}
		base = strings.TrimSpace(out)
	}

	diff, err := git(ctx, root, "diff", "--name-only", "--no-renames", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}

	var files []string
	for _, out := range []string{diff, untracked} {
		for name := range strings.SplitSeq(out, "\x00") {
			if name == "" {
				continue
			}
			files = append(files, filepath.Join(root, filepath.FromSlash(name)))
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
