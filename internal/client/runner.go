package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Runner struct {
	Config  Config
	API     *API
	Out     io.Writer
	prepare func(context.Context, string, Task) error
	judge   func(context.Context, string, VerdictQueue) (*Conclusion, string, string, error)
}

var prURLPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[1-9][0-9]{0,9}$`)

func validPR(url, sha string) bool {
	return prURLPattern.MatchString(url) && commitPattern.MatchString(sha)
}

func (r *Runner) workspaceFor(taskID string) string { return filepath.Join(r.Config.WorkDir, taskID) }

func (r *Runner) current(ctx context.Context, id string) (Task, error) {
	t, err := r.API.Task(ctx, id)
	if err != nil {
		return t, err
	}
	switch t.Status {
	case "open", "submitted", "rejected":
	default:
		return t, fmt.Errorf("task is %s; delivery/judgement stopped", t.Status)
	}
	if !t.Deadline.After(time.Now()) {
		return t, fmt.Errorf("task deadline has passed")
	}
	return t, nil
}

func (r *Runner) Run(ctx context.Context, id string, resume, rerun bool) error {
	if !ValidTaskID(id) {
		return fmt.Errorf("invalid task ID")
	}
	if r.Config.Kind != "worker" {
		return fmt.Errorf("this instance registered itself as %s; declaring work and delivering belong to a worker instance", r.Config.Kind)
	}
	if !resume {
		if err := r.validateCommand(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(r.Config.RunsDir, 0700); err != nil {
		return err
	}
	dir := filepath.Join(r.Config.RunsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	self, err := r.API.Self(ctx)
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, "state.json")
	var s State
	err = readJSON(statePath, &s)
	if os.IsNotExist(err) {
		if resume {
			return fmt.Errorf("no local state; use run to start this task")
		}
		s = State{Version: 2, Platform: r.API.Base, AgentID: self.ID, TaskID: id, Stage: "new"}
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("cannot read local state: %w", err)
	} else {
		if !resume {
			return fmt.Errorf("task already has local state; use resume")
		}
		if s.Version != 2 || s.Platform != r.API.Base || s.AgentID != self.ID || s.TaskID != id {
			return fmt.Errorf("local state belongs to another platform or Agent")
		}
	}
	t, err := r.API.Task(ctx, id)
	if err != nil {
		return err
	}
	if mine := r.myDelivery(t, self.ID, s); mine != nil {
		if s.PRURL != "" && mine.PRURL != s.PRURL || s.HeadSHA != "" && mine.HeadSHA != s.HeadSHA {
			return fmt.Errorf("platform already has a different delivery; inspect deliveries before resuming")
		}
		s.Stage = "delivered"
		s.DeliveryID = mine.ID
		s.PRURL, s.HeadSHA = mine.PRURL, mine.HeadSHA
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
		fmt.Fprintln(r.Out, "Delivered", t.ID, mine.StatusLabel())
		return nil
	}
	if s.Stage == "delivered" {
		return fmt.Errorf("local state says delivered but the platform has no such delivery; inspect deliveries")
	}
	if t.Status == "accepted" || t.Status == "cancelled" || !t.Deadline.After(time.Now()) {
		return fmt.Errorf("task is %s or expired; stopped", t.Status)
	}
	if s.Stage == "new" || s.Stage == "declared" || s.Stage == "ready" || s.Stage == "executing" || (s.Stage == "executed" && rerun) {
		if err = r.validateCommand(); err != nil {
			return err
		}
	}
	if s.Stage == "new" {
		if !validRepo(t.Repo, t.Commit) {
			return fmt.Errorf("unsupported repository or base commit")
		}
		attempt, err := r.API.Declare(ctx, id, r.Config.SelfReportSeconds)
		if err != nil {
			return fmt.Errorf("declaring work may be pending; retry with resume: %w", err)
		}
		s.Stage = "declared"
		s.Repo = t.Repo
		s.Commit = t.Commit
		s.EtaSeconds = attempt.EtaSeconds
		s.AttemptID = attempt.ID
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	}
	t, err = r.current(ctx, id)
	if err != nil {
		return err
	}
	if t.Repo != s.Repo || t.Commit != s.Commit {
		return fmt.Errorf("task repository/base commit changed")
	}
	if s.Stage == "declared" {
		fmt.Fprintln(r.Out, "Preparing", id)
		prepare := r.prepare
		if prepare == nil {
			prepare = r.prepareRepo
		}
		workCtx, cancel := context.WithDeadline(ctx, minTime(t.Deadline, time.Now().Add(time.Duration(r.Config.TimeoutSeconds)*time.Second)))
		err = prepare(workCtx, dir, t)
		cancel()
		if err != nil {
			return err
		}
		s.Stage = "ready"
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	}
	if s.Stage == "executing" && !rerun {
		return fmt.Errorf("Agent execution was interrupted; inspect workspace, then resume --rerun to explicitly execute again")
	}
	if s.Stage == "executed" && rerun {
		s.Stage = "executing"
	}
	if s.Stage == "ready" || s.Stage == "executing" {
		if _, err = r.current(ctx, id); err != nil {
			return err
		}
		output := filepath.Join(dir, "output")
		if _, err = os.Lstat(output); err == nil {
			if err = os.Rename(output, filepath.Join(dir, "output-"+randomID())); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err = os.Mkdir(output, 0700); err != nil {
			return err
		}
		if err = saveJSON(filepath.Join(dir, "task.json"), map[string]any{"id": t.ID, "title": t.Title, "requirement": t.Requirement, "repo_url": t.Repo, "base_commit": t.Commit, "auto_accept": t.AutoAccept, "deadline_at": t.Deadline}); err != nil {
			return err
		}
		s.Stage = "executing"
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
		fmt.Fprintln(r.Out, "Running Agent for", id)
		if err = r.execute(ctx, dir, t, self.ID); err != nil {
			return fmt.Errorf("Agent stopped; workspace retained: %w", err)
		}
		s.Stage = "executed"
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	}
	if s.Stage != "executed" && s.Stage != "validated" {
		return fmt.Errorf("unexpected local stage %q", s.Stage)
	}
	if s.Stage == "executed" {
		var ref struct {
			PRURL   string `json:"pr_url"`
			HeadSHA string `json:"head_sha"`
		}
		if err = readJSON(filepath.Join(dir, "output", "pr.json"), &ref); err != nil {
			return fmt.Errorf("open the Pull Request and write its reference to output/pr.json before submitting: %w", err)
		}
		if !validPR(ref.PRURL, strings.ToLower(ref.HeadSHA)) {
			return fmt.Errorf("invalid Pull Request reference in output/pr.json")
		}
		if err = r.checkDelivery(ctx, t.ID, s.Commit, ref.HeadSHA); err != nil {
			return err
		}
		summary := ""
		if b, err := readRegular(filepath.Join(dir, "output", "summary.txt"), 20000); err == nil {
			if !utf8.Valid(b) || strings.ContainsRune(string(b), '\x00') {
				return fmt.Errorf("summary must be UTF-8 without NUL")
			}
			summary = strings.TrimSpace(string(b))
		} else if !os.IsNotExist(err) {
			return err
		}
		for _, piece := range [][]byte{[]byte(ref.PRURL), []byte(ref.HeadSHA), []byte(summary)} {
			if bytesContains(piece, r.API.Key) {
				return fmt.Errorf("output contains the platform key; refusing to submit")
			}
		}
		s.PRURL, s.HeadSHA, s.Summary = ref.PRURL, strings.ToLower(ref.HeadSHA), summary
		s.Stage = "validated"
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	}
	if _, err = r.current(ctx, id); err != nil {
		return err
	}
	if _, err = r.current(ctx, id); err != nil {
		return err
	}
	d, err := r.API.Deliver(ctx, id, DeliveryInput{PRURL: s.PRURL, HeadSHA: s.HeadSHA, Summary: s.Summary})
	if err != nil {
		return fmt.Errorf("delivery outcome may be unknown; use resume to reconcile: %w", err)
	}
	if d.ID == "" || d.AgentID != self.ID || d.PRURL != s.PRURL || d.HeadSHA != s.HeadSHA {
		return fmt.Errorf("unexpected delivery response; use resume")
	}
	s.DeliveryID, s.Stage = d.ID, "delivered"
	if err = saveJSON(statePath, s); err != nil {
		return err
	}
	fmt.Fprintln(r.Out, "Delivered", id, d.StatusLabel())
	return nil
}

func bytesContains(b []byte, key string) bool {
	return key != "" && strings.Contains(string(b), key)
}

func (r *Runner) myDelivery(t Task, agentID string, s State) *Delivery {
	for i := range t.Deliveries {
		d := t.Deliveries[i]
		if d.AgentID != agentID {
			continue
		}
		if s.DeliveryID != "" && d.ID == s.DeliveryID {
			return &t.Deliveries[i]
		}
		if s.DeliveryID == "" && s.HeadSHA != "" && d.HeadSHA == s.HeadSHA {
			return &t.Deliveries[i]
		}
	}
	return nil
}

func (r *Runner) checkDelivery(ctx context.Context, taskID, commit, head string) error {
	work := r.workspaceFor(taskID)
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"merge-base", "--is-ancestor", commit, head}} {
		var out strings.Builder
		err := r.git(ctx, work, &out, args...)
		if args[0] == "rev-parse" {
			if err != nil {
				return err
			}
			if !strings.EqualFold(strings.TrimSpace(out.String()), head) {
				return fmt.Errorf("Pull Request head does not match the workspace HEAD")
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("base commit is not an ancestor of the Pull Request head: %w", err)
		}
	}
	return nil
}

func (r *Runner) Abandon(ctx context.Context, id, note string) error {
	if !ValidTaskID(id) {
		return fmt.Errorf("invalid task ID")
	}
	dir := filepath.Join(r.Config.RunsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	at, err := r.API.Abandon(ctx, id, note)
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, "state.json")
	var s State
	if err = readJSON(statePath, &s); err == nil && s.TaskID == id {
		s.Stage = "abandoned"
		if err = saveJSON(statePath, s); err != nil {
			return err
		}
	}
	fmt.Fprintln(r.Out, "Abandoned", at.TaskID, "attempt", at.ID)
	return nil
}

func (r *Runner) validateCommand() error {
	if len(r.Config.Command) == 0 || r.Config.Command[0] == "" {
		return fmt.Errorf("configure command before starting a task")
	}
	for _, arg := range r.Config.Command {
		if r.API.Key != "" && strings.Contains(arg, r.API.Key) {
			return fmt.Errorf("platform key must not appear in Agent command")
		}
	}
	joined := strings.Join(r.Config.Command, "\n")
	for _, placeholder := range []string{"{task}", "{workspace}", "{output}"} {
		if !strings.Contains(joined, placeholder) {
			return fmt.Errorf("Agent command must include %s", placeholder)
		}
	}
	if _, err := exec.LookPath(r.Config.Command[0]); err != nil {
		return fmt.Errorf("Agent executable %q not found; configure command first", r.Config.Command[0])
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("Git executable not found; install Git first")
	}
	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (r *Runner) execute(ctx context.Context, dir string, t Task, agent string) error {
	ctx, cancel := context.WithDeadline(ctx, minTime(t.Deadline, time.Now().Add(time.Duration(r.Config.TimeoutSeconds)*time.Second)))
	defer cancel()
	replacer := strings.NewReplacer("{task}", filepath.Join(dir, "task.json"), "{workspace}", r.workspaceFor(t.ID), "{output}", filepath.Join(dir, "output"))
	command := make([]string, len(r.Config.Command))
	for i, arg := range r.Config.Command {
		command[i] = replacer.Replace(arg)
	}
	done := make(chan error, 1)
	go func() {
		done <- runProcess(ctx, r.workspaceFor(t.ID), childEnv(r.API.Key, false), command, io.Discard)
	}()
	ticker := time.NewTicker(time.Duration(r.Config.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.current(ctx, t.ID); err != nil {
				cancel()
				<-done
				return fmt.Errorf("cannot confirm task is still worth working on: %w", err)
			}
		}
	}
}

func (d Delivery) StatusLabel() string {
	switch d.Status {
	case "pending":
		return "— awaiting the publisher's judgement"
	case "rejected":
		return "— rejected into the 打回区: " + d.Reason
	case "settled":
		return "— settled, this delivery took the whole bounty"
	default:
		return "— recorded as a supplementary solution"
	}
}

func (r *Runner) printTask(t Task) error {
	label := t.Status + fmt.Sprintf(" / bounty=%d in-flight=%d rounds=%d", t.Bounty, t.AgentsWorking, t.AttemptRounds)
	if t.SettledID != "" {
		label += " / settled"
	}
	for _, d := range t.Deliveries {
		if d.Status == "settled" || d.Status == "rejected" {
			label += " " + d.Status + "=" + d.SeqLabel()
		}
	}
	_, err := fmt.Fprintln(r.Out, t.ID, label)
	return err
}
func (d Delivery) SeqLabel() string {
	return fmt.Sprintf("pull #%s", strings.Split(d.PRURL, "/pull/")[1])
}

func (r *Runner) Status(ctx context.Context, id string, watch bool) error {
	if !ValidTaskID(id) {
		return fmt.Errorf("invalid task ID")
	}
	previous := ""
	for {
		t, err := r.API.Task(ctx, id)
		if err != nil {
			return err
		}
		key := fmt.Sprint(t.Status, len(t.Deliveries), t.SettledID)
		if previous != key {
			if err = r.printTask(t); err != nil {
				return err
			}
			previous = key
		}
		if !watch || t.Status == "accepted" || t.Status == "cancelled" {
			return nil
		}
		timer := time.NewTimer(time.Duration(r.Config.PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runner) Review(ctx context.Context, id string) error {
	if r.Config.Kind != "worker" {
		return fmt.Errorf("this instance registered itself as %s; only the key that made the delivery can buy a review", r.Config.Kind)
	}
	self, err := r.API.Self(ctx)
	if err != nil {
		return err
	}
	t, err := r.API.Task(ctx, id)
	if err != nil {
		return err
	}
	for _, d := range t.Deliveries {
		if d.AgentID == self.ID && d.Status == "rejected" && !d.Reviewed {
			reviewed, err := r.API.Review(ctx, id, d.ID)
			if err != nil {
				return fmt.Errorf("the review fee is burned unless the admin overturns the verdict; check the wallet before retrying: %w", err)
			}
			fmt.Fprintln(r.Out, "In the review pool:", id, reviewed.ID, "- outside the timeout queue until an admin speaks")
			return nil
		}
	}
	return fmt.Errorf("no rejected delivery of yours is waiting for a review on this task")
}
