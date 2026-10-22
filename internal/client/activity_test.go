package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivitySpoolRedactsSplitOutputAndRetainsAttempts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_TOKEN", "opaque-fixture-token")
	r := &Runner{Config: Config{RunsDir: dir}, API: NewAPI("http://localhost", "platform-secret"), Out: io.Discard}
	r.recordActivity(dir, "tsk_test", "push", "running", "info", "Starting push")
	w := &activityWriter{runner: r, dir: dir, task: "tsk_test"}
	_, _ = w.Write([]byte("opaque-fixture-"))
	_, _ = w.Write([]byte("token platform-secret\n"))
	w.Close()
	r.recordActivity(dir, "tsk_test", "push", "blocked", "error", "Denied")
	r.recordActivity(dir, "tsk_test", "push", "running", "info", "Retry")
	b, err := os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "opaque-fixture-token") || strings.Contains(string(b), "platform-secret") {
		t.Fatal("secret leaked into spool")
	}
	var events []ActivityEvent
	s := bufio.NewScanner(strings.NewReader(string(b)))
	for s.Scan() {
		var e ActivityEvent
		if err = json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 4 || events[3].Attempt != 2 || events[1].Step != "push" {
		t.Fatalf("bad step attempts: %#v", events)
	}
	// Loading the log in a fresh process retains monotonically increasing seq.
	r2 := &Runner{Config: r.Config, API: r.API, Out: io.Discard}
	r2.recordActivity(dir, "tsk_test", "push", "completed", "info", "Done")
	b, _ = os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if !strings.Contains(string(b), `"seq":5`) {
		t.Fatal("sequence was not recovered")
	}
}

func TestActivityOversizedLinesDoNotLeakTheirTrailingFragments(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Config: Config{RunsDir: dir}, API: NewAPI("http://localhost", "platform-secret"), Out: io.Discard}
	w := &activityWriter{runner: r, dir: dir, task: "tsk_test"}
	_, _ = w.Write([]byte(strings.Repeat("x", 33000) + "token-first-half"))
	_, _ = w.Write([]byte("secret-trailing-fragment\nsafe next line\n"))
	w.Close()
	b, err := os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-trailing-fragment") || !strings.Contains(string(b), "safe next line") {
		t.Fatal("oversized line was split into unsafe log fragments")
	}
}

func TestActivityReplayAdvancesOnlyAfterAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	var received []ActivityEvent
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/agent/activity" {
			t.Errorf("unexpected route %s", req.URL.Path)
		}
		if fail {
			http.Error(w, "temporary failure", 503)
			return
		}
		var in struct {
			Events []ActivityEvent `json:"events"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		received = append(received, in.Events...)
		w.WriteHeader(204)
	}))
	defer server.Close()
	r := &Runner{Config: Config{RunsDir: dir}, API: NewAPI(server.URL, "platform-secret"), Out: io.Discard}
	r.recordActivity(dir, "tsk_test", "prepare", "running", "info", "Checking out")
	r.syncActivity(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "activity.jsonl.cursor")); !os.IsNotExist(err) {
		t.Fatal("failed upload advanced cursor")
	}
	fail = false
	r.activity.retryAt = time.Time{}
	r.syncActivity(context.Background())
	r.syncActivity(context.Background())
	if len(received) != 1 || received[0].Seq != 1 {
		t.Fatalf("bad replay: %#v", received)
	}
}

func TestDefaultWorkspaceIsPersistentAndInstanceScoped(t *testing.T) {
	home := t.TempDir()
	if err := Init(home, "pi-dev", "worker", "pi-dev", "", ""); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := readJSON(filepath.Join(home, "pi-dev", "config.json"), &c); err != nil {
		t.Fatal(err)
	}
	if c.WorkDir != filepath.Join(home, "workspace", "pi-dev") {
		t.Fatalf("bad default %s", c.WorkDir)
	}
}

func TestActivityRecoveryMarksInterruptedStepsAndIsIdentityBound(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Config: Config{RunsDir: dir}, API: NewAPI("http://localhost", "old-key"), Out: io.Discard}
	r.recordActivity(dir, "tsk_test", "agent", "running", "info", "Working")
	r.markInterruptedActivity()
	b, _ := os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if !strings.Contains(string(b), `"status":"blocked"`) {
		t.Fatal("interrupted execution still appears running")
	}
	before := string(b)
	other := &Runner{Config: r.Config, API: NewAPI("http://localhost", "new-key"), Out: io.Discard}
	other.recordActivity(dir, "tsk_test", "agent", "running", "info", "Different identity")
	b, _ = os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if string(b) != before {
		t.Fatal("different identity modified an existing spool")
	}
	// Recover a crash-truncated final record without losing complete records.
	f, err := os.OpenFile(filepath.Join(dir, "activity.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":`)
	f.Close()
	restarted := &Runner{Config: r.Config, API: r.API, Out: io.Discard}
	restarted.recordActivity(dir, "tsk_test", "agent", "running", "info", "Explicitly resumed")
	b, _ = os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	scan := bufio.NewScanner(strings.NewReader(string(b)))
	for scan.Scan() {
		if !json.Valid(scan.Bytes()) {
			t.Fatal("incomplete record poisoned the spool")
		}
	}
}

func TestPublicationRecoveryPreservesOutputAndNeverRerunsModel(t *testing.T) {
	r, f := setupRunner(t)
	work := r.workspaceFor("tsk_test")
	t.Setenv("EMPORETIC_TEST_MODE", "publish-fail")
	if err := r.Run(context.Background(), "tsk_test", false, false); err == nil || !strings.Contains(err.Error(), "publication stopped") {
		t.Fatalf("expected publication failure: %v", err)
	}
	var state State
	dir := filepath.Join(r.Config.RunsDir, "tsk_test")
	if err := readJSON(filepath.Join(dir, "state.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.Stage != "publishing" || !state.PublishBlocked {
		t.Fatalf("bad recovery stage: %#v", state)
	}
	t.Setenv("EMPORETIC_TEST_MODE", "")
	if err := r.Run(context.Background(), "tsk_test", true, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "attempts"))
	if strings.Count(string(b), "run\n") != 1 {
		t.Fatalf("model ran again: %s", b)
	}
	if f.deliveries != 1 {
		t.Fatalf("wrong delivery count: %d", f.deliveries)
	}
	if _, err := os.Stat(filepath.Join(work, "code.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRetriesRespectBackoffAndAcquisitionWindow(t *testing.T) {
	r, f := setupRunner(t)
	dir := filepath.Join(r.Config.RunsDir, "tsk_test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := State{Stage: "publishing", PublishRetryAt: time.Now().Add(time.Minute)}
	if err := saveJSON(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	task := f.task
	task.Deadline = time.Now().Add(time.Minute)
	if list, _ := r.prescreen([]Task{task}); len(list) != 0 {
		t.Fatal("retried before backoff")
	}
	state.PublishRetryAt = time.Now().Add(-time.Second)
	_ = saveJSON(filepath.Join(dir, "state.json"), state)
	if list, _ := r.prescreen([]Task{task}); len(list) != 1 {
		t.Fatal("publishing should not require a full model execution window")
	}
	r.Config.PickCommand = []string{"missing-picker-must-not-run"}
	if chosen, reason := r.chooseTask([]Task{task}); chosen == nil || chosen.ID != task.ID {
		t.Fatalf("publication recovery went through title screening: %s", reason)
	}
}
