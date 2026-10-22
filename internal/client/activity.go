package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Activity is observational: it never changes the delivery state machine.
type ActivityEvent struct {
	RunID   string    `json:"run_id"`
	TaskID  string    `json:"task_id"`
	Seq     int64     `json:"seq"`
	Step    string    `json:"step"`
	Attempt int       `json:"attempt"`
	Status  string    `json:"status"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type activityStream struct {
	seq      int64
	step     string
	attempts map[string]int
	statuses map[string]string
}
type activityState struct {
	mu         sync.Mutex
	upload     sync.Mutex
	runningCtx context.Context
	streams    map[string]*activityStream
	wake       chan struct{}
	paths      map[string]bool
	retryAt    time.Time
}

var activitySteps = map[string]bool{"prepare": true, "github": true, "fork": true, "agent": true, "push": true, "pr": true, "deliver": true, "awaiting_verdict": true, "judge": true, "verdict": true}
var activitySecret = regexp.MustCompile(`(?i)(github_pat_[a-z0-9_]+|gh[pousr]_[a-z0-9_]+|https?://[^\s/@]+:[^\s/@]+@|(?:bearer|basic)\s+[a-z0-9+/=_-]+)`)

func (r *Runner) scrubActivity(s string) string {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if len(value) >= 8 && (strings.HasSuffix(name, "TOKEN") || strings.HasSuffix(name, "SECRET") || strings.HasSuffix(name, "PASSWORD") || strings.HasSuffix(name, "API_KEY") || strings.HasSuffix(name, "ACCESS_KEY")) {
			s = strings.ReplaceAll(s, value, "[redacted]")
		}
	}
	for _, key := range []string{r.API.Key, os.Getenv("EMPORETIC_GITHUB_TOKEN"), os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")} {
		if key != "" {
			s = strings.ReplaceAll(s, key, "[redacted]")
		}
	}
	// Also scrub the opaque token from the local token file, not only PAT formats.
	tokenFile := os.Getenv("EMPORETIC_GITHUB_TOKEN_FILE")
	if tokenFile == "" && r.Config.HomeDir != "" {
		tokenFile = filepath.Join(r.Config.HomeDir, "github-token")
	}
	if b, err := os.ReadFile(tokenFile); err == nil {
		if token := strings.TrimSpace(string(b)); token != "" {
			s = strings.ReplaceAll(s, token, "[redacted]")
		}
	}
	s = activitySecret.ReplaceAllString(s, "[redacted]")
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
	if len(s) > 4000 {
		s = s[:4000]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}

func (r *Runner) recordActivity(dir, task, step, status, level, message string) {
	a := &r.activity
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.streams == nil {
		a.streams = map[string]*activityStream{}
	}
	if a.paths == nil {
		a.paths = map[string]bool{}
	}
	path := filepath.Join(dir, "activity.jsonl")
	ownerPath := filepath.Join(dir, "activity-owner.json")
	owner := struct{ Platform, KeyHash string }{r.API.Base, hash([]byte(r.API.Key))}
	var savedOwner struct{ Platform, KeyHash string }
	if err := readJSON(ownerPath, &savedOwner); os.IsNotExist(err) {
		if saveJSON(ownerPath, owner) != nil {
			return
		}
	} else if err != nil || savedOwner != owner {
		return
	}
	stream := a.streams[path]
	if stream == nil {
		stream = &activityStream{attempts: map[string]int{}, statuses: map[string]string{}, step: "agent"}
		if f, err := os.Open(path); err == nil {
			scan := bufio.NewScanner(f)
			scan.Buffer(make([]byte, 8192), 65536)
			for scan.Scan() {
				var e ActivityEvent
				if json.Unmarshal(scan.Bytes(), &e) == nil {
					stream.seq = max(stream.seq, e.Seq)
					stream.attempts[e.Step] = max(stream.attempts[e.Step], e.Attempt)
					stream.step = e.Step
					if e.Status != "" {
						stream.statuses[e.Step] = e.Status
					}
				}
			}
			f.Close()
			// A crash may leave one incomplete final record. It was never
			// acknowledged; discard only that suffix before appending again.
			if tail, e := os.Open(path); e == nil {
				if info, e := tail.Stat(); e == nil && info.Size() > 0 {
					size := min(int64(65536), info.Size())
					data := make([]byte, size)
					if _, e = tail.ReadAt(data, info.Size()-size); e == nil && data[len(data)-1] != '\n' {
						if index := strings.LastIndexByte(string(data), '\n'); index >= 0 {
							_ = os.Truncate(path, info.Size()-size+int64(index)+1)
						} else if size == info.Size() {
							_ = os.Truncate(path, 0)
						}
					}
				}
				tail.Close()
			}
		}
		a.streams[path] = stream
	}
	if step == "" {
		step = stream.step
	}
	if !activitySteps[step] {
		step = "agent"
	}
	if status == "running" && stream.statuses[step] != "running" {
		stream.attempts[step]++
	}
	if status != "" {
		stream.step = step
		stream.statuses[step] = status
	}
	attempt := max(1, stream.attempts[step])
	if level != "error" && level != "tool" {
		level = "info"
	}
	rel, _ := filepath.Rel(r.Config.RunsDir, dir)
	e := ActivityEvent{RunID: hash([]byte(rel)), TaskID: task, Seq: stream.seq + 1, Step: step, Attempt: attempt, Status: status, Level: level, Message: r.scrubActivity(message), At: time.Now().UTC()}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(r.Out, "Could not save activity log:", filepath.Base(dir))
		return
	}
	stream.seq = e.Seq
	a.paths[path] = true
	if a.wake != nil {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
}

// Flush lines through one writer so stdout/stderr fragments cannot interleave.
type activityWriter struct {
	mu        sync.Mutex
	runner    *Runner
	dir, task string
	level     string
	pending   []byte
	dropping  bool
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.pending = append(w.pending, p...)
	for {
		i := strings.IndexByte(string(w.pending), '\n')
		if i < 0 {
			break
		}
		if !w.dropping {
			w.line(w.pending[:i])
		}
		w.dropping = false
		w.pending = w.pending[i+1:]
	}
	// Discard the entire oversized line, including future fragments: retaining
	// only its suffix could expose part of a token across a truncation boundary.
	if len(w.pending) > 32768 {
		w.line([]byte("[long output line omitted; adapter should emit newline-delimited logs]"))
		w.pending = nil
		w.dropping = true
	}
	return n, nil
}
func (w *activityWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 && !w.dropping {
		w.line(w.pending)
		w.pending = nil
	}
}
func (w *activityWriter) line(b []byte) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return
	}
	if strings.HasPrefix(s, "EMPORETIC_EVENT ") {
		var e struct{ Step, Status, Level, Message string }
		if json.Unmarshal([]byte(strings.TrimPrefix(s, "EMPORETIC_EVENT ")), &e) == nil && activitySteps[e.Step] && (e.Status == "" || e.Status == "running" || e.Status == "completed" || e.Status == "blocked" || e.Status == "waiting") {
			w.runner.recordActivity(w.dir, w.task, e.Step, e.Status, e.Level, e.Message)
			return
		}
	}
	w.runner.recordActivity(w.dir, w.task, "", "", w.level, s)
}

func (r *Runner) startActivitySync(ctx context.Context) {
	r.activity.mu.Lock()
	if r.activity.runningCtx != nil && r.activity.runningCtx.Err() == nil {
		r.activity.mu.Unlock()
		return
	}
	r.activity.runningCtx = ctx
	if r.activity.wake == nil {
		r.activity.wake = make(chan struct{}, 1)
	}
	wake := r.activity.wake
	r.activity.mu.Unlock()
	go func() {
		// Recover unsent logs after a daemon restart. Never descend into code or outputs.
		_ = filepath.WalkDir(r.Config.RunsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && path != r.Config.RunsDir && (strings.HasPrefix(d.Name(), "output") || strings.HasPrefix(d.Name(), "judging-output") || d.Name() == "checkout" || d.Name() == "publish" || d.Name() == ".git") {
				return filepath.SkipDir
			}
			if !d.IsDir() && d.Type().IsRegular() && d.Name() == "activity.jsonl" {
				r.activity.mu.Lock()
				if r.activity.paths == nil {
					r.activity.paths = map[string]bool{}
				}
				r.activity.paths[path] = true
				r.activity.mu.Unlock()
			}
			return nil
		})
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			r.syncActivity(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake: // coalesce bursts without blocking execution
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
	}()
}

// Called only while holding the resident daemon lock, before starting any work.
// An interrupted model stays interrupted; observing it never reruns the model.
func (r *Runner) markInterruptedActivity() {
	_ = filepath.WalkDir(r.Config.RunsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != r.Config.RunsDir && (strings.HasPrefix(d.Name(), "output") || strings.HasPrefix(d.Name(), "judging-output") || d.Name() == "checkout" || d.Name() == "publish") {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() || d.Name() != "activity.jsonl" {
			return nil
		}
		f, e := os.Open(path)
		if e != nil {
			return nil
		}
		defer f.Close()
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 8192), 65536)
		var last ActivityEvent
		for scan.Scan() {
			var event ActivityEvent
			if json.Unmarshal(scan.Bytes(), &event) == nil && event.Status != "" {
				last = event
			}
		}
		if last.Status == "running" {
			r.recordActivity(filepath.Dir(path), last.TaskID, last.Step, "blocked", "error", "Previous daemon stopped during this step; inspect saved output and resume from its checkpoint")
		}
		return nil
	})
}

func (r *Runner) syncActivity(ctx context.Context) {
	if !r.activity.upload.TryLock() {
		return
	}
	defer r.activity.upload.Unlock()
	if time.Now().Before(r.activity.retryAt) {
		return
	}
	r.activity.mu.Lock()
	paths := make([]string, 0, len(r.activity.paths))
	for p := range r.activity.paths {
		paths = append(paths, p)
	}
	r.activity.mu.Unlock()
	for _, path := range paths {
		var owner struct{ Platform, KeyHash string }
		if readJSON(filepath.Join(filepath.Dir(path), "activity-owner.json"), &owner) != nil || owner.Platform != r.API.Base || owner.KeyHash != hash([]byte(r.API.Key)) {
			continue
		}
		var cursor struct {
			Offset int64 `json:"offset"`
		}
		cursorPath := path + ".cursor"
		_ = readJSON(cursorPath, &cursor)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if info, e := f.Stat(); e == nil && cursor.Offset > info.Size() {
			cursor.Offset = 0
		}
		if _, err = f.Seek(cursor.Offset, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		reader := bufio.NewReader(f)
		events := []ActivityEvent{}
		offset := cursor.Offset
		for len(events) < 100 {
			b, e := reader.ReadBytes('\n')
			if e != nil {
				break
			}
			var event ActivityEvent
			if json.Unmarshal(b, &event) != nil {
				break
			}
			events = append(events, event)
			offset += int64(len(b))
		}
		f.Close()
		if len(events) == 0 {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = r.API.JSON(requestCtx, "POST", "/agent/activity", map[string]any{"events": events}, "", nil)
		cancel()
		if err != nil {
			r.activity.retryAt = time.Now().Add(10 * time.Second)
			return
		} // The local cursor advances only after acknowledgement.
		cursor.Offset = offset
		_ = saveJSON(cursorPath, cursor)
	}
}
