package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real executable adapter: tests exercise process management, environment filtering,
// Pull Request validation, persisted state and HTTP rather than mocking the whole runner.
func TestAgentHelper(t *testing.T) {
	if os.Getenv("EMPORETIC_TEST_AGENT") != "1" {
		return
	}
	args := os.Args
	out := args[len(args)-1]
	taskPath, work := args[len(args)-3], args[len(args)-2]
	if os.Getenv("EMPORETIC_TEST_MODE") == "pick" { // 标题筛只喂瘦身字段：它连工作区都不进。
		taskPath, work = args[len(args)-2], ""
	}
	if os.Getenv("EMPORETIC_API_KEY") != "" || os.Getenv("TEST_PLATFORM_KEY") != "" || os.Getenv("KEY_ALIAS") != "" {
		os.Exit(10)
	}
	if work != "" {
		f, err := os.OpenFile(filepath.Join(filepath.Dir(out), "attempts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(12)
		}
		if os.Getenv("EMPORETIC_PUBLISH_ONLY") != "1" {
			_, _ = f.WriteString("run\n")
		}
		_ = f.Close()
	}
	if os.Getenv("EMPORETIC_TEST_MODE") == "pick" { // 标题筛只回候选 ID 与跳过理由。
		var brief struct {
			Candidates []struct {
				ID string `json:"id"`
			} `json:"candidates"`
		}
		if err := readJSON(taskPath, &brief); err != nil || len(brief.Candidates) == 0 {
			os.Exit(25)
		}
		picks := map[string]any{"ids": []string{brief.Candidates[len(brief.Candidates)-1].ID},
			"skipped": []map[string]string{{"id": brief.Candidates[0].ID, "reason": "仓库不是熟路"}}}
		if err := saveJSON(filepath.Join(out, "picks.json"), picks); err != nil {
			os.Exit(26)
		}
		os.Exit(0)
	}
	var task Task
	if err := readJSON(taskPath, &task); err != nil {
		os.Exit(11)
	}
	if os.Getenv("EMPORETIC_PUBLISH_ONLY") == "1" {
		var checkpoint struct {
			Head string `json:"head_sha"`
		}
		if readJSON(filepath.Join(out, "publish-checkpoint.json"), &checkpoint) != nil {
			os.Exit(31)
		}
		if saveJSON(filepath.Join(out, "pr.json"), map[string]string{"pr_url": "https://github.com/example/project/pull/1", "head_sha": checkpoint.Head}) != nil {
			os.Exit(32)
		}
		os.Exit(0)
	}
	switch os.Getenv("EMPORETIC_TEST_MODE") {
	case "fail":
		os.Exit(4)
	case "sleep":
		time.Sleep(30 * time.Second)
	}
	if err := os.WriteFile(filepath.Join(work, "code.txt"), []byte("fixed\n"), 0600); err != nil {
		os.Exit(13)
	}
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("new file\n"), 0600); err != nil {
		os.Exit(14)
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = work
		return cmd.CombinedOutput()
	}
	if b, err := git("add", "-A"); err != nil {
		os.Exit(15)
	} else if len(bytes.TrimSpace(b)) > 0 {
		os.Exit(16)
	}
	if b, err := git("-c", "advice.detachedHead=false", "commit", "-q", "-m", "fix"); err != nil {
		os.Exit(19)
	} else if len(b) > 0 {
		os.Exit(20)
	}
	var head bytes.Buffer
	if b, err := git("rev-parse", "HEAD"); err != nil {
		os.Exit(21)
	} else {
		head.Write(b)
	}
	pr := map[string]string{"pr_url": "https://github.com/example/project/pull/1", "head_sha": strings.TrimSpace(head.String())}
	if os.Getenv("EMPORETIC_TEST_MODE") == "publish-fail" {
		_ = saveJSON(filepath.Join(out, "publish-checkpoint.json"), map[string]any{"version": 1, "task_id": task.ID, "repo_url": task.Repo, "base_commit": task.Commit, "head_sha": pr["head_sha"]})
		_ = saveJSON(filepath.Join(out, "publish-error.json"), map[string]any{"retryable": false, "message": "permission denied"})
		os.Exit(33)
	}
	switch os.Getenv("EMPORETIC_TEST_MODE") {
	case "stale-head":
		pr["head_sha"] = strings.Repeat("a", 40)
	case "invalid":
		pr["pr_url"] = "https://github.com/example/project/pull/not-a-number"
	}
	if err := saveJSON(filepath.Join(out, "pr.json"), pr); err != nil {
		os.Exit(22)
	}
	if err := os.WriteFile(filepath.Join(out, "summary.txt"), []byte("Fixed and added a regression file."), 0600); err != nil {
		os.Exit(23)
	}
	os.Exit(0)
}

type fakePlatform struct {
	mu                                     sync.Mutex
	task                                   Task
	attempts, deliveries, publishes, beats int
	pings, verdicts                        int
	staleHead                              bool
	loseAttempt, loseDelivery, losePublish bool
	publishKey, publishBody                string
	nextProbeID                            string
	mineList, availList                    []Task
	abandons                               int
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-secret-platform" {
		http.Error(w, "unauthorized", 401)
		return
	}
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	id := r.Header.Get("Idempotency-Key")
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/agent":
		// 假平台只回一格 kind：身份在注册时定死，测试里要哪一边就在自己的夹具里改那一格。
		write(Agent{ID: "agt_test", OwnerID: "usr_owner", Name: "work-agent", Kind: "worker"})
	case r.Method == "GET" && r.URL.Path == "/api/v1/tasks/tsk_test":
		write(f.task)
	case r.Method == "GET" && r.URL.Path == "/api/v1/tasks":
		switch r.URL.Query().Get("scope") {
		case "mine":
			write(f.mineList)
		default:
			write(f.availList)
		}
	case r.Method == "POST" && r.URL.Path == "/api/v1/tasks/tsk_test/abandon":
		f.abandons++
		var in struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Note == "" {
			http.Error(w, `{"error":{"code":"INVALID_INPUT"}}`, 422)
			return
		}
		write(Attempt{ID: "att_abandoned", TaskID: "tsk_test", AbandonedAt: ptr(time.Now())})
	case r.Method == "POST" && r.URL.Path == "/api/v1/tasks/tsk_test/attempts":
		f.attempts++
		if f.loseAttempt {
			f.loseAttempt = false
			http.Error(w, "response lost", 500)
			return
		}
		write(Attempt{ID: "att_test", TaskID: "tsk_test", EtaSeconds: 14400})
	case r.Method == "POST" && r.URL.Path == "/api/v1/tasks/tsk_test/deliveries":
		f.deliveries++
		var in DeliveryInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		d := Delivery{ID: "sub_test", TaskID: "tsk_test", AgentID: "agt_test", PRURL: in.PRURL, HeadSHA: in.HeadSHA, Summary: in.Summary, Seq: int64(f.deliveries), CreatedAt: time.Now(), Eligible: true, Status: "pending"}
		if f.staleHead {
			d.HeadSHA = strings.Repeat("b", 40)
		}
		f.task.Deliveries = append(f.task.Deliveries, d)
		f.task.Status = "submitted"
		if f.loseDelivery {
			f.loseDelivery = false
			http.Error(w, "lost delivery response", 500)
			return
		}
		write(d)
	case r.Method == "POST" && r.URL.Path == "/api/v1/agent/heartbeat":
		f.beats++
		var in struct {
			Seq int64 `json:"seq"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Seq <= int64(f.beats-1) {
			http.Error(w, `{"error":{"code":"STALE_HEARTBEAT"}}`, 409)
			return
		}
		write(map[string]any{"seq": in.Seq, "arrived_at": time.Now()})
	case r.Method == "GET" && r.URL.Path == "/api/v1/probes/pending":
		if f.nextProbeID == "" {
			w.WriteHeader(204)
			return
		}
		write(Probe{ID: f.nextProbeID})
	case strings.HasPrefix(r.Method+" "+r.URL.Path, "POST /api/v1/probes/") && strings.HasSuffix(r.URL.Path, "/pong"):
		f.pings++
		write(map[string]any{"rtt_ms": 7})
	case r.Method == "POST" && r.URL.Path == "/api/v1/tasks/tsk_test/verdicts/poll":
		f.verdicts++
		if len(f.task.Deliveries) == 0 {
			w.WriteHeader(204)
			return
		}
		write(VerdictQueue{Delivery: f.task.Deliveries[0], Requirement: f.task.Requirement, Repo: f.task.Repo, Commit: f.task.Commit})
	case strings.HasPrefix(r.Method+" "+r.URL.Path, "POST /api/v1/tasks/tsk_test/deliveries/sub_test/verdict"):
		f.verdicts++
		var in struct {
			Status     string      `json:"status"`
			Conclusion *Conclusion `json:"conclusion"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Conclusion.Items) == 0 {
			http.Error(w, `{"error":{"code":"INVALID_INPUT"}}`, 422)
			return
		}
		f.task.Status = "accepted"
		f.task.SettledID = "sub_test"
		write(f.task)
	case r.Method == "POST" && r.URL.Path == "/api/v1/tasks":
		b, _ := io.ReadAll(r.Body)
		if f.publishKey != "" && (f.publishKey != id || f.publishBody != string(b)) {
			http.Error(w, "different retry", 409)
			return
		}
		if f.publishKey == "" {
			f.publishes++
			f.publishKey = id
			f.publishBody = string(b)
		}
		if f.losePublish {
			f.losePublish = false
			http.Error(w, "lost publish response", 500)
			return
		}
		write(Task{ID: "tsk_published"})
	default:
		http.Error(w, "unexpected route", 404)
	}
}
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func setupRunner(t *testing.T) (*Runner, *fakePlatform) {
	t.Helper()
	t.Setenv("EMPORETIC_TEST_AGENT", "1")
	t.Setenv("EMPORETIC_API_KEY", "test-secret-platform")
	t.Setenv("TEST_PLATFORM_KEY", "test-secret-platform")
	t.Setenv("KEY_ALIAS", "test-secret-platform")
	repo := t.TempDir()
	gitTest(t, repo, "init", "--template=")
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("bug\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "code.txt")
	gitTest(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "base")
	commit := gitTest(t, repo, "rev-parse", "HEAD")
	f := &fakePlatform{task: Task{ID: "tsk_test", Title: "Fix", Requirement: "Make the failing test green", Repo: "https://github.com/example/project", Commit: commit, Bounty: 300, Deadline: time.Now().Add(time.Hour), Status: "open"}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: Config{Key: "test-secret-platform", Kind: "worker", RunsDir: t.TempDir(), WorkDir: filepath.Join(t.TempDir(), "work"), Command: []string{exe, "-test.run=^TestAgentHelper$", "--", "{task}", "{workspace}", "{output}"}, PollSeconds: 1, TimeoutSeconds: 10, SelfReportSeconds: 14400, HeartbeatSeconds: 15}, API: NewAPI(server.URL, "test-secret-platform"), Out: io.Discard}
	r.prepare = func(ctx context.Context, _ string, task Task) error {
		// 工作文件夹只剩代码：检出落在 <workspace>/<task-id>，日志与断点留在身份目录里。
		if err := os.MkdirAll(r.Config.WorkDir, 0755); err != nil {
			return err
		}
		gitTest(t, r.Config.WorkDir, "clone", "--no-hardlinks", repo, task.ID)
		return nil
	}
	return r, f
}
func requireError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("expected error containing %q, got %v", contains, err)
	}
}
func attempts(t *testing.T, r *Runner) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Config.RunsDir, "tsk_test", "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run\n")
}

// TestDeliveryLostResponseRecovery 覆盖交付响应丢失：resume 用状态对账，不重复执行也不重复交付。
func TestDeliveryLostResponseRecovery(t *testing.T) {
	r, f := setupRunner(t)
	f.loseDelivery = true
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "outcome may be unknown")
	if err := r.Run(context.Background(), "tsk_test", true, false); err != nil {
		t.Fatal(err)
	}
	if f.deliveries != 1 || attempts(t, r) != 1 {
		t.Fatalf("duplicate work: %#v", f)
	}
	var s State
	if err := readJSON(filepath.Join(r.Config.RunsDir, "tsk_test", "state.json"), &s); err != nil || s.Stage != "delivered" {
		t.Fatal(s, err)
	}
	for _, name := range []string{"task.json", "state.json"} {
		b, _ := os.ReadFile(filepath.Join(r.Config.RunsDir, "tsk_test", name))
		if bytes.Contains(b, []byte(r.API.Key)) {
			t.Fatal("key in state/input")
		}
	}
}

// TestAttemptCrashRecovery：交付以 Pull Request 引用收尾，声明开工是第一个可能丢响应的调用。
func TestAttemptCrashRecovery(t *testing.T) {
	r, f := setupRunner(t)
	f.loseAttempt = true
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "declaring work may be pending")
	if err := r.Run(context.Background(), "tsk_test", true, false); err != nil {
		t.Fatal(err)
	}
	if attempts(t, r) != 1 || f.deliveries != 1 {
		t.Fatalf("unexpected repeated execution/delivery: %#v", f)
	}
}
func TestInterruptedAgentRequiresExplicitRerun(t *testing.T) {
	r, f := setupRunner(t)
	t.Setenv("EMPORETIC_TEST_MODE", "fail")
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "Agent stopped")
	requireError(t, r.Run(context.Background(), "tsk_test", true, false), "--rerun")
	t.Setenv("EMPORETIC_TEST_MODE", "")
	if err := r.Run(context.Background(), "tsk_test", true, true); err != nil {
		t.Fatal(err)
	}
	if attempts(t, r) != 2 || f.deliveries != 1 {
		t.Fatal("wrong attempt count")
	}
}

// TestInvalidPRReferenceNeverDelivers：没有 PR 引用或 head SHA 不建立在 base_commit 之上都不算交付。
func TestInvalidPRReferenceNeverDelivers(t *testing.T) {
	r, f := setupRunner(t)
	t.Setenv("EMPORETIC_TEST_MODE", "invalid")
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "invalid Pull Request reference")
	if f.deliveries != 0 {
		t.Fatal("invalid reference delivered anyway")
	}
}
func TestStaleHeadCannotBeReported(t *testing.T) {
	r, f := setupRunner(t)
	t.Setenv("EMPORETIC_TEST_MODE", "stale-head")
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "does not match the workspace HEAD")
	if f.deliveries != 0 {
		t.Fatal("reported a head the workspace does not have")
	}
}

// TestCancellationStopsExecutingAgent 覆盖任务被取消之后必须立刻停手。
func TestCancellationStopsExecutingAgent(t *testing.T) {
	r, f := setupRunner(t)
	t.Setenv("EMPORETIC_TEST_MODE", "sleep")
	timer := time.AfterFunc(500*time.Millisecond, func() { f.mu.Lock(); f.task.Status = "cancelled"; f.mu.Unlock() })
	defer timer.Stop()
	start := time.Now()
	if err := r.Run(context.Background(), "tsk_test", false, false); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Agent not stopped promptly")
	}
}
func TestDeadlineStopsAgent(t *testing.T) {
	r, f := setupRunner(t)
	t.Setenv("EMPORETIC_TEST_MODE", "sleep")
	f.task.Deadline = time.Now().Add(700 * time.Millisecond)
	start := time.Now()
	if err := r.Run(context.Background(), "tsk_test", false, false); err == nil {
		t.Fatal("expected deadline")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("deadline not respected")
	}
}
func TestLockSurvivesFileAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	release, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, e := lockFile(path); e == nil {
		other()
		t.Fatal("concurrent lock succeeded")
	}
	release()
	release, err = lockFile(path)
	if err != nil {
		t.Fatal("stale file blocked recovery", err)
	}
	release()
}

// TestHeartbeatIsMonotonicAndAnswersPing 覆盖心跳签到与主动探测。
func TestHeartbeatIsMonotonicAndAnswersPing(t *testing.T) {
	r, f := setupRunner(t)
	if err := r.Beat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.beats != 1 || f.pings != 0 {
		t.Fatalf("heartbeat/probe: %#v", f)
	}
	f.nextProbeID = "prb_test"
	if err := r.Beat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.beats != 2 || f.pings != 1 {
		t.Fatalf("probe must be answered with a pong: %#v", f)
	}
}

// TestJudgePostsStructuredConclusion 覆盖 issuer 实例：拉取待验收交付并回传分项结论。
func TestJudgePostsStructuredConclusion(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	f.staleHead = true
	d := Delivery{ID: "sub_test", TaskID: "tsk_test", AgentID: "agt_test", PRURL: "https://github.com/example/project/pull/9", HeadSHA: strings.Repeat("b", 40), Status: "pending"}
	f.mu.Lock()
	f.task.Deliveries = []Delivery{d}
	f.task.Status = "submitted"
	f.mu.Unlock()
	r.judge = func(_ context.Context, _ string, q VerdictQueue) (*Conclusion, string, string, error) {
		return &Conclusion{Items: []ConclusionItem{{Requirement: q.Requirement, Outcome: "met", Evidence: []string{"diff 第 3-9 行"}}}}, "completed", "对照需求满足", nil
	}
	if err := r.JudgeOnce(context.Background(), "tsk_test"); err != nil {
		t.Fatal(err)
	}
	if f.verdicts < 2 {
		t.Fatalf("poll and verdict both happened: %v", f.verdicts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.task.Status != "accepted" || f.task.SettledID != "sub_test" {
		t.Fatalf("judgement did not settle: %+v", f.task)
	}
}

func TestPublishRetryKeepsFileAndIdempotencyKey(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	f.losePublish = true
	dir := t.TempDir()
	in := TaskInput{Title: "Fix", Requirement: "Fix bug", Repo: f.task.Repo, Commit: f.task.Commit, Bounty: 50, Deadline: time.Now().Add(time.Hour)}
	path := filepath.Join(dir, "task.json")
	if err := saveJSON(path, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(context.Background(), path, "intent-1", true); err == nil {
		t.Fatal("expected uncertain response")
	}
	if err := r.Publish(context.Background(), path, "intent-1", true); err != nil {
		t.Fatal(err)
	}
	if f.publishes != 1 || f.publishKey == "" {
		t.Fatal("duplicate publish or a test package upload that no longer exists")
	}
	in.Title = "changed"
	_ = saveJSON(path, in)
	requireError(t, r.Publish(context.Background(), path, "intent-1", true), "contents changed")
}

// 幂等指纹完全来自这份输入本身。
func TestPublishIsTheWholePayload(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	dir := t.TempDir()
	in := TaskInput{Title: "Fix", Requirement: "Fix bug", Repo: f.task.Repo, Commit: f.task.Commit, Bounty: 80, Deadline: time.Now().Add(2 * time.Hour)}
	path := filepath.Join(dir, "task.json")
	if err := saveJSON(path, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(context.Background(), path, "intent-one-shot", true); err != nil {
		t.Fatal(err)
	}
	if f.publishes != 1 {
		t.Fatalf("the input is the whole payload: %#v", f)
	}
}
func TestNoCrossOriginRedirect(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	a := NewAPI(server.URL, "secret")
	if err := a.JSON(context.Background(), "GET", "/agent", nil, "", nil); err == nil {
		t.Fatal("followed redirect")
	}
	if leaked {
		t.Fatal("request leaked")
	}
}
func TestConfigTransportAndPaths(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	const instance = "pi-dev"
	path := filepath.Join(home, instance, "config.json")
	if err := Init(home, instance, "worker", instance, "我的 pi", work); err != nil {
		t.Fatal(err)
	}
	if err := Init(home, instance, "worker", instance, "", work); err == nil {
		t.Fatal("overwrote config or registered the same instance twice")
	}
	list, err := ListInstances(home)
	if err != nil || len(list) != 1 || list[0].Kind != "worker" || list[0].Label == "" {
		t.Fatalf("registry.json is the machine inventory: %+v %v", list, err)
	}
	c := Config{APIURL: "http://127.0.0.1:8000", Key: "test-secret-platform", Kind: "worker", Name: instance,
		WorkDir: work, Command: []string{"my-agent", "--task", "{task}", "--workspace", "{workspace}", "--output", "{output}"},
		PollSeconds: 10, TimeoutSeconds: 3600, SelfReportSeconds: 14400, HeartbeatSeconds: 60}
	if err = saveJSON(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(home, instance)
	if err != nil || loaded.RunsDir != filepath.Join(home, instance, "runs") || loaded.WorkDir != work || loaded.SelfReportSeconds != 14400 || loaded.HeartbeatSeconds != 60 {
		t.Fatal(loaded, err)
	}
	// key 住在身份目录里：骨架先空着，去网站登记之后把它贴进来。
	c.Key = "test-secret-platform"
	for _, url := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/api/v1"} {
		c.APIURL = url
		_ = saveJSON(path, c)
		if _, err = LoadConfig(home, instance); err == nil {
			t.Fatal("accepted", url)
		}
	}
	c.APIURL = "http://127.0.0.1:8000"
	c.Key = ""
	_ = saveJSON(path, c)
	if _, err = LoadConfig(home, instance); err == nil {
		t.Fatal("accepted an instance without a platform key")
	}
	c.Key = "test-secret-platform"
	c.Kind = "hybrid" // 第三种人不存在：翻完不必仍是任何一个 preset，这不是自由。
	_ = saveJSON(path, c)
	if _, err = LoadConfig(home, instance); err == nil {
		t.Fatal("accepted a third identity")
	}
	c.Kind = "worker"
	// 开工窗口自报上限 24 小时，心跳节奏也不能太离谱。
	for _, bad := range []struct {
		self, beat int
	}{
		{299, 60}, {86401, 60}, {14400, 14}, {14400, 3601}} {
		c.SelfReportSeconds, c.HeartbeatSeconds = bad.self, bad.beat
		_ = saveJSON(path, c)
		if _, err = LoadConfig(home, instance); err == nil {
			t.Fatal("accepted self_report_seconds/heartbeat_seconds", bad)
		}
	}
	for _, id := range []string{"../escape", "tsk_../../escape", "tsk_x?x=1", "tsk_x/attempts"} {
		if ValidTaskID(id) {
			t.Fatal("unsafe task ID", id)
		}
	}
}

func TestMissingExecutableDoesNotDeclareWork(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Command[0] = "emporetic-nonexistent-executable"
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "not found")
	if f.attempts != 0 {
		t.Fatal("declared work before validating Agent executable")
	}
}

func TestCheckoutRenameCrashRecovery(t *testing.T) {
	r, f := setupRunner(t)
	dir := filepath.Join(r.Config.RunsDir, "tsk_test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.prepare(context.Background(), dir, f.task); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(dir, "checkout.json"), struct{ Repo, Commit string }{f.task.Repo, f.task.Commit}); err != nil {
		t.Fatal(err)
	}
	if err := r.prepareRepo(context.Background(), dir, f.task); err != nil {
		t.Fatal(err)
	}
	wrong := f.task
	wrong.Commit = strings.Repeat("a", 40)
	requireError(t, r.prepareRepo(context.Background(), dir, wrong), "untracked workspace")
}

// TestJudgeSkipsEmptyVerdictQueue 覆盖空验收队列：平台回 204 时判定侧直接返回，不能碰 NULL。
func TestJudgeSkipsEmptyVerdictQueue(t *testing.T) {
	r, _ := setupRunner(t)
	r.Config.Kind = "issuer"
	r.judge = func(context.Context, string, VerdictQueue) (*Conclusion, string, string, error) {
		t.Fatal("judgement ran on an empty queue")
		return nil, "", "", nil
	}
	if err := r.JudgeOnce(context.Background(), "tsk_test"); err != nil {
		t.Fatal(err)
	}
}

// TestPublishThenDaemonRoundJudgesNothing 覆盖发单之后的第一拍：自己刚发的 open 任务名下还没有交付，轮询不能崩。
func TestPublishThenDaemonRoundJudgesNothing(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	f.mu.Lock()
	mine := f.task
	mine.Status = "open"
	f.mineList = []Task{mine}
	f.availList = nil
	f.mu.Unlock()
	r.Config.JudgeCommand = []string{"my-judge", "--delivery", "{task}", "--output", "{output}"}
	r.judge = func(_ context.Context, _ string, q VerdictQueue) (*Conclusion, string, string, error) {
		t.Fatalf("the judgement command ran for an empty queue: %+v", q.Delivery)
		return nil, "", "", nil
	}
	r.cycle(context.Background())
	if f.verdicts != 1 { // poll 拉了一次、回传一次也没得回
		t.Fatalf("empty verdict queue: %v", f.verdicts)
	}
}

// TestCycleSkipsJudgementWithoutJudgeCommand 覆盖 issuer 实例没配 judge_command：不空拉验收队列，也不每个 tick 刷一行错误。
func TestCycleSkipsJudgementWithoutJudgeCommand(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	f.mu.Lock()
	submitted := f.task
	submitted.Status = "submitted"
	f.mineList = []Task{submitted}
	f.mu.Unlock()
	r.cycle(context.Background())
	if f.verdicts != 0 {
		t.Fatalf("pulled the verdict queue without judge_command: %v", f.verdicts)
	}
}

// TestDaemonStartsWithoutRunsDir 覆盖新机器/新目录：runs_dir 还没人创建时 daemon 也要能常驻（lockFile 不建目录）。
func TestDaemonStartsWithoutRunsDir(t *testing.T) {
	r, _ := setupRunner(t)
	r.Config.RunsDir = filepath.Join(t.TempDir(), "fresh-runs")
	ctx, stop := context.WithCancel(context.Background())
	stop()
	done := make(chan error, 1)
	go func() { done <- r.Daemon(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon outlived cancellation")
	}
	if _, err := os.Stat(filepath.Join(r.Config.RunsDir, ".daemon.lock")); err != nil {
		t.Fatal("no daemon lock", err)
	}
}

// TestAbandonWithoutLocalState 覆盖在另一台机器上开工之后放弃尝试：本地没有任务目录也得能 abandon。
func TestAbandonWithoutLocalState(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.RunsDir = filepath.Join(t.TempDir(), "fresh-runs")
	if err := r.Abandon(context.Background(), "tsk_test", "干不动了"); err != nil {
		t.Fatal(err)
	}
	if f.abandons != 1 {
		t.Fatalf("abandon never reached the platform: %#v", f.abandons)
	}
}

// TestCycleResumesItsOwnInterruptedTask 覆盖常驻挑单：本地留过记录的任务要从断点接上，而不是卡在「use resume」之外。
func TestCycleResumesItsOwnInterruptedTask(t *testing.T) {
	r, f := setupRunner(t)
	dir := filepath.Join(r.Config.RunsDir, "tsk_test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	if err := saveJSON(filepath.Join(dir, "state.json"), State{Version: 2, Platform: r.API.Base, AgentID: "agt_test", TaskID: "tsk_test", Stage: "ready", Repo: f.task.Repo, Commit: f.task.Commit, HeadSHA: sha}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.task.Status = "submitted"
	f.task.Deliveries = []Delivery{{ID: "sub_test", TaskID: "tsk_test", AgentID: "agt_test", PRURL: "https://github.com/example/project/pull/9", HeadSHA: sha, Status: "pending", CreatedAt: time.Now()}}
	f.availList = []Task{f.task}
	f.mu.Unlock()
	r.cycle(context.Background())
	var s State
	if err := readJSON(filepath.Join(dir, "state.json"), &s); err != nil || s.Stage != "delivered" {
		t.Fatalf("interrupted work was not resumed: %+v %v", s, err)
	}
	if f.attempts != 0 {
		t.Fatal("resumed work declared a second attempt")
	}
}

func ptr[T any](v T) *T { return &v }

// TestIdentitiesDoNotCross 覆盖两种身份互不越权：接单的实例发不了单，验收的实例开不了工。
func TestIdentitiesDoNotCross(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Kind = "issuer"
	requireError(t, r.Run(context.Background(), "tsk_test", false, false), "registered itself as issuer")
	if f.attempts != 0 {
		t.Fatal("an issuer instance declared work")
	}
	r.Config.Kind = "worker"
	dir := t.TempDir()
	in := TaskInput{Title: "Fix", Requirement: "Fix bug", Repo: f.task.Repo, Commit: f.task.Commit, Bounty: 50, Deadline: time.Now().Add(time.Hour)}
	path := filepath.Join(dir, "task.json")
	if err := saveJSON(path, in); err != nil {
		t.Fatal(err)
	}
	requireError(t, r.Publish(context.Background(), path, "intent-worker-hands", true), "registered itself as worker")
	requireError(t, r.JudgeOnce(context.Background(), "tsk_test"), "registered itself as worker")
	if f.publishes != 0 || f.verdicts != 0 {
		t.Fatalf("a worker instance published or judged anyway: %#v", f)
	}
}

// TestWorkerPickHonorsFiltersAndTitleScreen 覆盖两段漏斗：硬条件先把不可能的挡在模型之外，模型只回候选 ID。
func TestWorkerPickHonorsFiltersAndTitleScreen(t *testing.T) {
	r, f := setupRunner(t)
	r.Config.Intent = "这类活我要做"
	r.Config.Filters.RepoKeywords = []string{"github.com/example"}
	r.Config.Filters.MinBounty = 100
	r.Config.TimeoutSeconds = 3600
	r.Config.PickCommand = nil
	f.mu.Lock()
	long := time.Now().Add(6 * time.Hour)
	soon := time.Now().Add(30 * time.Minute)
	f.availList = []Task{
		{ID: "tsk_low", Title: "小额悬赏", Repo: "https://github.com/example/project", Bounty: 80, Deadline: long},
		{ID: "tsk_other", Title: "别的仓库", Repo: "https://github.com/other/repo", Bounty: 500, Deadline: long},
		{ID: "tsk_rushed", Title: "来不及跑完", Repo: "https://github.com/example/project", Bounty: 500, Deadline: soon},
		{ID: "tsk_pick", Title: "吃得下那一单", Repo: "https://github.com/example/project", Bounty: 500, Deadline: long},
	}
	f.mu.Unlock()

	candidates, reason := r.prescreen(f.availList)
	if len(candidates) != 1 || candidates[0].ID != "tsk_pick" {
		t.Fatalf("mechanical filters let the wrong work through: %v (%s)", candidates, reason)
	}
	next, why := r.chooseTask(candidates)
	if next == nil || next.ID != "tsk_pick" {
		t.Fatalf("title screen without a local model takes the first survivor: %v (%s)", next, why)
	}
}
