package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var repoPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func validRepo(repo, commit string) bool {
	return repoPattern.MatchString(repo) && commitPattern.MatchString(commit)
}
func (r *Runner) git(ctx context.Context, dir string, out io.Writer, args ...string) error {
	cmd := []string{"git", "-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never"}
	return runProcess(ctx, dir, childEnv(r.API.Key, true), append(cmd, args...), out)
}
func (r *Runner) prepareRepo(ctx context.Context, dir string, t Task) error {
	if !validRepo(t.Repo, t.Commit) {
		return fmt.Errorf("only public HTTPS GitHub repositories and full 40-character commits are supported")
	}
	work := r.workspaceFor(t.ID)
	if _, err := os.Lstat(work); err == nil {
		var prepared struct{ Repo, Commit string }
		if err = readJSON(filepath.Join(dir, "checkout.json"), &prepared); err != nil || prepared.Repo != t.Repo || prepared.Commit != t.Commit {
			return fmt.Errorf("untracked workspace exists; preserve it and move it aside before resuming")
		}
		var head bytes.Buffer
		if err = r.git(ctx, work, &head, "rev-parse", "HEAD"); err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(head.String()), t.Commit) {
			return fmt.Errorf("prepared checkout no longer matches base commit")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	staging := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	steps := [][]string{{"init", "--template="}, {"fetch", "--no-tags", "--depth=1", t.Repo, t.Commit}, {"checkout", "--detach", "--force", t.Commit}}
	for _, args := range steps {
		if err := r.git(ctx, staging, io.Discard, args...); err != nil {
			return fmt.Errorf("repository preparation failed: %w", err)
		}
	}
	var head bytes.Buffer
	if err := r.git(ctx, staging, &head, "rev-parse", "HEAD"); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(head.String()), t.Commit) {
		return fmt.Errorf("checkout does not match base commit")
	}
	if err := saveJSON(filepath.Join(dir, "checkout.json"), struct{ Repo, Commit string }{t.Repo, t.Commit}); err != nil {
		return err
	}
	return os.Rename(staging, work)
}
