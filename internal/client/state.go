package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var taskIDPattern = regexp.MustCompile(`^tsk_[A-Za-z0-9_-]{1,100}$`)

func ValidTaskID(id string) bool { return taskIDPattern.MatchString(id) }
func hash(b []byte) string       { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func readJSON(path string, v any) error {
	b, err := readRegular(path, 1<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func readRegular(path string, max int64) ([]byte, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() || i.Size() > max {
		return nil, fmt.Errorf("%s must be a regular file of at most %d bytes", filepath.Base(path), max)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("file too large")
	}
	return b, nil
}

type State struct {
	PublishAttempts int       `json:"publish_attempts,omitempty"`
	PublishBlocked  bool      `json:"publish_blocked,omitempty"`
	PublishRetryAt  time.Time `json:"publish_retry_at,omitempty"`
	Version         int       `json:"version"`
	Platform        string    `json:"platform"`
	AgentID         string    `json:"agent_id"`
	TaskID          string    `json:"task_id"`
	Stage           string    `json:"stage"`
	Repo            string    `json:"repo_url"`
	Commit          string    `json:"base_commit"`
	EtaSeconds      int       `json:"eta_seconds,omitempty"`
	AttemptID       string    `json:"attempt_id,omitempty"`
	PRURL           string    `json:"pr_url,omitempty"`
	HeadSHA         string    `json:"head_sha,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	DeliveryID      string    `json:"delivery_id,omitempty"`
}
