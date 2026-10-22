package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func (r *Runner) heartbeatPath() string { return filepath.Join(r.Config.RunsDir, "heartbeat.json") }
func (r *Runner) daemonLock() string    { return filepath.Join(r.Config.RunsDir, ".daemon.lock") }

func readSeq(path string) (int64, error) {
	var v struct{ Seq int64 }
	if err := readJSON(path, &v); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return v.Seq, nil
}

func (r *Runner) Beat(ctx context.Context) error {
	if err := os.MkdirAll(r.Config.RunsDir, 0700); err != nil {
		return err
	}
	// Also serialize daemon --once against the resident daemon's heartbeats.
	unlock, err := lockFile(filepath.Join(r.Config.RunsDir, ".heartbeat.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := r.heartbeatPath()
	seq, err := readSeq(path)
	if err != nil {
		return err
	}
	if err = r.sendHeartbeat(ctx, path, seq); err != nil {
		return err
	}
	id, err := r.API.PendingProbe(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if err = r.API.Pong(ctx, id); err != nil {
		return fmt.Errorf("pong failed: %w", err)
	}
	fmt.Fprintln(r.Out, "Pong answered probe", id)
	return nil
}

func (r *Runner) sendHeartbeat(ctx context.Context, path string, seq int64) error {
	for attempt := 0; attempt < 2; attempt++ {
		if seq < 0 || seq == math.MaxInt64 {
			return fmt.Errorf("heartbeat sequence is invalid or exhausted")
		}
		seq++
		// Reserve before sending: a timeout may hide a committed heartbeat.
		// Seq records the last allocated attempt, not the last acknowledged one.
		if err := saveJSON(path, struct{ Seq int64 }{seq}); err != nil {
			return err
		}
		release, err := r.API.heartbeatRelease(ctx, seq)
		if err == nil {
			r.release = release
			fmt.Fprintf(r.Out, "Heartbeat seq=%d ok\n", seq)
			return nil
		}
		var apiErr *APIError
		if attempt == 0 && errors.As(err, &apiErr) && apiErr.Status == 409 && apiErr.Code == "STALE_HEARTBEAT" {
			floor := time.Now().UnixMilli() // Compatibility with platforms predating last_seq.
			if apiErr.LastSeq != nil {
				floor = *apiErr.LastSeq
			}
			seq = max(seq, floor)
			fmt.Fprintf(r.Out, "Heartbeat sequence stale; resync after seq=%d\n", seq)
			continue
		}
		return fmt.Errorf("heartbeat failed; consecutive failures start counting here: %w", err)
	}
	return fmt.Errorf("heartbeat sequence recovery exhausted")
}

type judgementState struct {
	Platform, KeyHash, InputHash string
	Queue                        VerdictQueue
	Conclusion                   *Conclusion
	Status, Reason               string
	Delivered                    bool
}

func validJudgement(c *Conclusion, status, reason string) bool {
	if (status != "completed" && status != "incomplete") || c == nil || len(c.Items) == 0 || len(c.Items) > 20 || len(reason) > 2000 || !utf8.ValidString(reason) || strings.ContainsRune(reason, '\x00') {
		return false
	}
	valid := func(s string, n int) bool {
		return strings.TrimSpace(s) != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsRune(s, '\x00')
	}
	for _, item := range c.Items {
		if !valid(item.Requirement, 500) || (item.Outcome != "met" && item.Outcome != "not_met") || len(item.Evidence) > 20 {
			return false
		}
		for _, e := range item.Evidence {
			if !valid(e, 2000) {
				return false
			}
		}
	}
	return true
}

func (r *Runner) JudgeOnce(ctx context.Context, taskID string) error {
	if r.Config.Kind != "issuer" {
		return fmt.Errorf("this instance registered itself as %s; judging belongs to the issuer that posted this bounty", r.Config.Kind)
	}
	if !ValidTaskID(taskID) {
		return fmt.Errorf("invalid task ID")
	}
	dir := filepath.Join(r.Config.RunsDir, taskID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	queue, err := r.API.PollVerdict(ctx, taskID)
	if err != nil {
		return err
	}
	if queue == nil {
		return nil
	}
	if queue.Delivery.ID == "" {
		return fmt.Errorf("platform returned no delivery identity")
	}
	dir = filepath.Join(dir, "judgements", hash([]byte(queue.Delivery.ID)))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Dispatch metadata may change between reads; fingerprint only immutable inputs.
	input, _ := json.Marshal(struct{ ID, Repo, Commit, Requirement, PR, Head, Summary string }{queue.Delivery.ID, queue.Repo, queue.Commit, queue.Requirement, queue.Delivery.PRURL, queue.Delivery.HeadSHA, queue.Delivery.Summary})
	path := filepath.Join(dir, "state.json")
	state := judgementState{}
	if err = readJSON(path, &state); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot read saved judgement: %w", err)
	}
	if err == nil && (state.Platform != r.API.Base || state.KeyHash != hash([]byte(r.API.Key)) || state.InputHash != hash(input)) {
		return fmt.Errorf("saved judgement belongs to another identity or input")
	}
	if os.IsNotExist(err) {
		state = judgementState{Platform: r.API.Base, KeyHash: hash([]byte(r.API.Key)), InputHash: hash(input), Queue: *queue}
		if err = saveJSON(path, state); err != nil {
			return err
		}
	}
	judge := r.judge
	if judge == nil {
		judge = r.runJudgeCommand
	}
	if state.Status == "" {
		conclusion, status, reason, e := judge(ctx, dir, *queue)
		if e != nil {
			return e
		}
		if !validJudgement(conclusion, status, reason) {
			return fmt.Errorf("judgement does not satisfy the platform conclusion format")
		}
		state.Conclusion, state.Status, state.Reason = conclusion, status, reason
		if err = saveJSON(path, state); err != nil {
			return err
		}
	}
	if !validJudgement(state.Conclusion, state.Status, state.Reason) {
		return fmt.Errorf("saved judgement is invalid; inspect its state.json")
	}
	t, err := r.API.PostVerdict(ctx, taskID, queue.Delivery.ID, state.Status, state.Conclusion, state.Reason)
	if err != nil {
		return fmt.Errorf("verdict outcome may be unknown; retry the same judgement for this delivery: %w", err)
	}
	state.Delivered = true
	if err = saveJSON(path, state); err != nil {
		return err
	}
	fmt.Fprintln(r.Out, "Judged", taskID, queue.Delivery.ID, state.Status, "->", t.Status)
	return nil
}

func (r *Runner) runJudgeCommand(ctx context.Context, dir string, q VerdictQueue) (*Conclusion, string, string, error) {
	if len(r.Config.JudgeCommand) == 0 || r.Config.JudgeCommand[0] == "" {
		return nil, "", "", fmt.Errorf("configure judge_command on this issuer instance before accepting deliveries")
	}
	for _, arg := range r.Config.JudgeCommand {
		if r.API.Key != "" && strings.Contains(arg, r.API.Key) {
			return nil, "", "", fmt.Errorf("platform key must not appear in the judgement command")
		}
	}
	brief := map[string]any{"delivery": q.Delivery, "requirement": q.Requirement, "repo_url": q.Repo, "base_commit": q.Commit}
	if err := saveJSON(filepath.Join(dir, "judging.json"), brief); err != nil {
		return nil, "", "", err
	}
	out, err := os.MkdirTemp(dir, "judging-output-")
	if err != nil {
		return nil, "", "", err
	}
	replacer := strings.NewReplacer("{task}", filepath.Join(dir, "judging.json"), "{output}", out)
	command := make([]string, len(r.Config.JudgeCommand))
	for i, arg := range r.Config.JudgeCommand {
		command[i] = replacer.Replace(arg)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(r.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := runProcess(runCtx, dir, childEnv(r.API.Key, false), command, io.Discard); err != nil {
		return nil, "", "", fmt.Errorf("judgement Agent stopped: %w", err)
	}
	var result struct {
		Status     string     `json:"status"`
		Conclusion Conclusion `json:"conclusion"`
		Reason     string     `json:"reason"`
	}
	if err := readJSON(filepath.Join(out, "verdict.json"), &result); err != nil {
		return nil, "", "", fmt.Errorf("the judgement command must write output/verdict.json with a structured conclusion: %w", err)
	}
	if !validJudgement(&result.Conclusion, result.Status, result.Reason) {
		return nil, "", "", fmt.Errorf("verdict must contain a valid status, reason and itemised findings")
	}
	conclusion := result.Conclusion
	return &conclusion, result.Status, result.Reason, nil
}

func (r *Runner) Daemon(ctx context.Context) error {
	if err := os.MkdirAll(r.Config.RunsDir, 0700); err != nil {
		return err
	}
	unlock, err := lockFile(r.daemonLock())
	if err != nil {
		return err
	}
	defer unlock()
	ticker := time.NewTicker(time.Duration(r.Config.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	poll := time.NewTicker(time.Duration(r.Config.PollSeconds) * time.Second)
	defer poll.Stop()
	stage := r.stageRelease
	if stage == nil {
		stage = stageUpdate
	}
	activate := r.activateRelease
	if activate == nil {
		activate = activateUpdate
	}
	workDone := make(chan struct{}, 1)
	updates := make(chan updateResult, 1)
	busy, downloading := false, false
	var ready string
	var nextCheck time.Time
	startWork := func() {
		if busy || ready != "" {
			return
		}
		busy = true
		go func() { r.cycle(ctx); workDone <- struct{}{} }()
	}
	checkUpdate := func() {
		if r.release == nil || !newer(r.release.Version, Version) || time.Now().Before(nextCheck) {
			return
		}
		if r.Config.AutoUpdate != nil && !*r.Config.AutoUpdate {
			fmt.Fprintln(r.Out, "Stable release available:", r.release.Version, "(auto_update=false)")
			nextCheck = time.Now().Add(time.Hour)
			return
		}
		if downloading || ready != "" {
			return
		}
		downloading = true
		release := *r.release
		go func() { path, err := stage(ctx, release); updates <- updateResult{path, err} }()
	}
	beat := func() {
		if err := r.Beat(ctx); err != nil {
			fmt.Fprintln(r.Out, "Beat:", err)
		}
		checkUpdate()
	}
	beat()
	startWork()
	for {
		if ready != "" && !busy && ctx.Err() == nil {
			if err := activate(ctx, ready); err != nil {
				fmt.Fprintln(r.Out, "Update deferred:", err)
				ready = ""
				r.updatePending.Store(false)
				nextCheck = time.Now().Add(time.Hour)
			}
		}
		select {
		case <-ctx.Done():
			if busy {
				<-workDone
			}
			return nil
		case <-workDone:
			busy = false
		case result := <-updates:
			downloading = false
			nextCheck = time.Now().Add(time.Hour)
			if result.err != nil {
				fmt.Fprintln(r.Out, "Update deferred:", result.err)
			} else {
				ready = result.path
				r.updatePending.Store(true)
				fmt.Fprintln(r.Out, "Update ready; waiting for current work to finish")
			}
		case <-ticker.C:
			beat()
		case <-poll.C:
			startWork()
		}
	}

}

func (r *Runner) cycle(ctx context.Context) {
	if r.Config.Kind == "issuer" {
		r.judgeQueue(ctx)
		return
	}
	r.pickWork(ctx)
}

func (r *Runner) judgeQueue(ctx context.Context) {
	if len(r.Config.JudgeCommand) == 0 || r.Config.JudgeCommand[0] == "" {
		return
	}
	mine, err := r.API.MineTasks(ctx)
	if err != nil {
		fmt.Fprintln(r.Out, "tasks mine:", err)
		return
	}
	for _, t := range mine {
		if r.updatePending.Load() {
			return
		}
		if t.Status != "submitted" && t.Status != "rejected" && t.Status != "open" {
			continue
		}
		if err := r.JudgeOnce(ctx, t.ID); err != nil {
			fmt.Fprintln(r.Out, "judge", t.ID+":", err)
		}
	}
}

func (r *Runner) pickWork(ctx context.Context) {
	available, err := r.API.AvailableTasks(ctx)
	if err != nil {
		fmt.Fprintln(r.Out, "tasks available:", err)
		return
	}
	candidates, reason := r.prescreen(available)
	if len(candidates) == 0 {
		fmt.Fprintln(r.Out, "skipping the open bounty list:", reason)
		return
	}
	for len(candidates) > 0 && ctx.Err() == nil {
		if r.updatePending.Load() {
			return
		}
		next, reason := r.chooseTask(candidates)
		if next == nil {
			fmt.Fprintln(r.Out, "skipping the open bounty list:", reason)
			return
		}
		var s State
		resume := readJSON(filepath.Join(r.Config.RunsDir, next.ID, "state.json"), &s) == nil && s.TaskID == next.ID
		fmt.Fprintf(r.Out, "Working on %s (%d credits, %d in flight)\n", next.ID, next.Bounty, next.AgentsWorking)
		if err := r.Run(ctx, next.ID, resume, false); err == nil {
			return
		} else {
			fmt.Fprintln(r.Out, "run", next.ID+":", err)
		}
		chosenID := next.ID
		remaining := candidates[:0]
		for _, candidate := range candidates {
			if candidate.ID != chosenID {
				remaining = append(remaining, candidate)
			}
		}
		candidates = remaining
	}
}

func (r *Runner) prescreen(tasks []Task) ([]Task, string) {
	window := time.Duration(r.Config.TimeoutSeconds) * time.Second
	floor := time.Now().Add(window)
	left := time.Duration(r.Config.Filters.MinTimeLeftSec) * time.Second
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if !ValidTaskID(t.ID) {
			continue
		}
		var state State
		err := readJSON(filepath.Join(r.Config.RunsDir, t.ID, "state.json"), &state)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(r.Out, "skip", t.ID+": local state needs inspection")
			continue
		}
		if err == nil {
			switch state.Stage {
			case "new", "declared", "ready", "executed", "validated":
			default:
				continue // Delivered, abandoned and interrupted executions need an explicit user decision.
			}
		}
		if !t.Deadline.After(floor) || !t.Deadline.After(time.Now().Add(left)) || t.Bounty < r.Config.Filters.MinBounty {
			continue
		}
		if len(r.Config.Filters.RepoKeywords) > 0 {
			matched := false
			for _, keyword := range r.Config.Filters.RepoKeywords {
				if keyword != "" && strings.Contains(strings.ToLower(t.Repo), strings.ToLower(keyword)) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, "no open bounty passes this instance's filters"
	}
	return out, ""
}

func (r *Runner) chooseTask(tasks []Task) (*Task, string) {
	ids, err := r.titleScreen(tasks)
	if err != nil {
		return nil, fmt.Sprintf("title screening failed: %v", err)
	}
	for i := range tasks {
		if len(r.Config.PickCommand) > 0 && !containsID(ids, tasks[i].ID) {
			continue
		}
		return &tasks[i], ""
	}
	return nil, "the local model skipped every candidate"
}

func containsID(ids []string, id string) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}

func (r *Runner) titleScreen(tasks []Task) ([]string, error) {
	if len(r.Config.PickCommand) == 0 {
		return nil, nil
	}
	if r.Config.PickCommand[0] == "" {
		return nil, fmt.Errorf("configure a screening executable")
	}
	dir := filepath.Join(r.Config.RunsDir, "screening")
	if err := os.MkdirAll(filepath.Join(dir, "output"), 0700); err != nil {
		return nil, err
	}
	candidates := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		candidates = append(candidates, map[string]any{"id": t.ID, "title": t.Title, "repo_url": t.Repo,
			"bounty_credits": t.Bounty, "deadline_at": t.Deadline, "agents_working": t.AgentsWorking, "attempt_rounds": t.AttemptRounds})
	}
	brief := map[string]any{"intent": r.Config.Intent, "candidates": candidates}
	if err := saveJSON(filepath.Join(dir, "screening.json"), brief); err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer("{screening}", filepath.Join(dir, "screening.json"), "{output}", filepath.Join(dir, "output"))
	command := make([]string, len(r.Config.PickCommand))
	for i, arg := range r.Config.PickCommand {
		command[i] = replacer.Replace(arg)
	}
	picksPath := filepath.Join(dir, "output", "picks.json")
	if err := os.Remove(picksPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot clear previous screening result: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := runProcess(ctx, dir, childEnv(r.API.Key, false), command, io.Discard); err != nil {
		return nil, fmt.Errorf("title screening stopped: %w", err)
	}
	var picks struct {
		IDs     []string `json:"ids"`
		Skipped []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := readJSON(picksPath, &picks); err != nil {
		return nil, fmt.Errorf("the screening command must write output/picks.json with candidate ids and skip reasons: %w", err)
	}
	for _, skip := range picks.Skipped {
		fmt.Fprintln(r.Out, "skip", skip.ID+":", skip.Reason)
	}
	return picks.IDs, nil
}

func (r *Runner) Once(ctx context.Context) error {
	if err := r.Beat(ctx); err != nil {
		return err
	}
	r.cycle(ctx)
	return nil
}
