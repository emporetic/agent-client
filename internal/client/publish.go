package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TaskInput struct {
	Title         string    `json:"title"`
	Requirement   string    `json:"requirement"`
	Repo          string    `json:"repo_url"`
	Commit        string    `json:"base_commit"`
	AutoAccept    *bool     `json:"auto_accept,omitempty"`
	CountsInStats *bool     `json:"counts_in_stats,omitempty"`
	Filters       any       `json:"filters,omitempty"`
	Bounty        int64     `json:"bounty_credits"`
	Deadline      time.Time `json:"deadline_at"`
}
type PublishState struct {
	Platform, AgentID, Fingerprint, Key string
	Input                               TaskInput
	TaskID                              string
}

func (r *Runner) Publish(ctx context.Context, inputPath, requestID string, countsInStats bool) error {
	if r.Config.Kind != "issuer" {
		return fmt.Errorf("this instance registered itself as %s; publishing, topping up and extending belong to an issuer instance", r.Config.Kind)
	}
	data, err := readRegular(inputPath, 1<<20)
	if err != nil {
		return err
	}
	var in TaskInput
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("task input must contain one JSON object")
	}
	counts := countsInStats
	in.CountsInStats = &counts
	if !validRepo(in.Repo, in.Commit) || in.Bounty < 1 || in.Bounty > 1000000000 || strings.TrimSpace(in.Title) == "" || len(in.Title) > 200 || strings.TrimSpace(in.Requirement) == "" || len(in.Requirement) > 20000 {
		return fmt.Errorf("invalid task requirements, repository, commit, or budget")
	}
	canonical, _ := json.Marshal(in)
	fingerprint := hash(canonical)
	if requestID == "" {
		requestID = fingerprint
	}
	dir := filepath.Join(r.Config.RunsDir, "publish", hash([]byte(requestID)))
	if err = os.MkdirAll(dir, 0700); err != nil {
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
	path := filepath.Join(dir, "state.json")
	var s PublishState
	if err = readJSON(path, &s); os.IsNotExist(err) {
		if !in.Deadline.After(time.Now()) {
			return fmt.Errorf("deadline must be in the future")
		}
		s = PublishState{Platform: r.API.Base, AgentID: self.ID, Fingerprint: fingerprint, Key: randomID(), Input: in}
		if err = saveJSON(path, s); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if s.Platform != r.API.Base || s.AgentID != self.ID || s.Fingerprint != fingerprint {
		return fmt.Errorf("publish request belongs to another Agent/platform or its contents changed; restore original input for retry")
	}
	if s.TaskID != "" {
		fmt.Fprintln(r.Out, "Published", s.TaskID)
		return nil
	}
	var task Task
	if err = r.API.JSON(ctx, "POST", "/tasks", s.Input, s.Key, &task); err != nil {
		return fmt.Errorf("publish may be pending; retry the same command and original files: %w", err)
	}
	if !ValidTaskID(task.ID) {
		return fmt.Errorf("publish returned no valid task ID; retry same command")
	}
	s.TaskID = task.ID
	if err = saveJSON(path, s); err != nil {
		return err
	}
	fmt.Fprintln(r.Out, "Published", task.ID)
	return nil
}
