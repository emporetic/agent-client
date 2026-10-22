package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStableVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v1.10.0", "v1.9.9", true}, {"v2.0.0", "v1.99.99", true}, {"v1.0.0", "v1.0.0", false},
		{"v1.0.0-rc.1", "v0.9.0", false}, {"v1.0.0", "dev", false}, {"v01.0.0", "v0.9.0", false}, {"v0.9.0", "v1.0.0", false},
	} {
		if newer(tc.a, tc.b) != tc.want {
			t.Errorf("newer(%s,%s)", tc.a, tc.b)
		}
	}
}
func testArchive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestBundleExtractionRejectsUnsafeEntries(t *testing.T) {
	for _, h := range []*tar.Header{
		{Name: "versions/v1.2.3/../../../escape", Typeflag: tar.TypeReg, Size: 1},
		{Name: "versions/v1.2.3/examples/link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
		{Name: "versions/v1.2.3/hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"},
		{Name: "versions/v9.9.9/emporetic-agent", Typeflag: tar.TypeReg, Size: 1},
	} {
		t.Run(h.Name, func(t *testing.T) {
			if err := extractBundle(bytes.NewReader(testArchive(t, []*tar.Header{h})), t.TempDir(), "v1.2.3"); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestBundleExtractionAndAtomicPointer(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "versions", "v1.2.3")
	if err := os.MkdirAll(dest, 0755); err != nil {
		t.Fatal(err)
	}
	data := testArchive(t, []*tar.Header{
		{Name: "current", Typeflag: tar.TypeSymlink, Linkname: "versions/v1.2.3"},
		{Name: "versions/", Typeflag: tar.TypeDir},
		{Name: "versions/v1.2.3/emporetic-agent", Typeflag: tar.TypeReg, Size: 3},
		{Name: "versions/v1.2.3/examples/a.mjs", Typeflag: tar.TypeReg, Size: 2},
	})
	if err := extractBundle(bytes.NewReader(data), dest, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "emporetic-agent"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("binary permissions: %v %v", info, err)
	}
	old := filepath.Join(root, "versions", "v1.0.0")
	if err := os.MkdirAll(old, 0755); err != nil {
		t.Fatal(err)
	}
	if err := switchCurrent(root, "versions/v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := switchCurrent(root, "versions/v1.2.3"); err != nil {
		t.Fatal(err)
	}
	dest, err = filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil || resolved != dest {
		t.Fatalf("pointer: %s %v", resolved, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("old version removed")
	}
}
func TestAutoUpdateConfiguration(t *testing.T) {
	for _, extra := range []string{"", `,"auto_update":false`, `,"auto_update":true`} {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, "test"), 0700); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"api_url":"https://example.com","api_key":"test","kind":"worker","workspace_dir":%q%s}`, home, extra)
		if err := os.WriteFile(filepath.Join(home, "test", "config.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(home, "test")
		if err != nil {
			t.Fatal(err)
		}
		enabled := c.AutoUpdate == nil || *c.AutoUpdate
		if enabled != (extra != `,"auto_update":false`) {
			t.Fatalf("wrong default: %s", extra)
		}
	}
}
func TestDaemonWaitsForWorkAndKeepsHeartbeatsDuringUpdate(t *testing.T) {
	old := Version
	Version = "v1.0.0"
	defer func() { Version = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	finish := make(chan struct{})
	staged := make(chan struct{})
	activated := make(chan struct{}, 1)
	var once sync.Once
	var beats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/heartbeat":
			beats.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"client_update": Release{Version: "v1.1.0"}})
		case "/api/v1/probes/pending":
			w.WriteHeader(204)
		case "/api/v1/tasks":
			once.Do(func() { close(entered) })
			select {
			case <-finish:
			case <-r.Context().Done():
			}
			io.WriteString(w, "[]")
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	r := heartbeatRunner(t.TempDir(), srv.URL)
	r.Config.Kind = "issuer"
	r.Config.JudgeCommand = []string{"judge"}
	r.Config.HeartbeatSeconds = 1
	r.Config.PollSeconds = 1
	r.stageRelease = func(context.Context, Release) (string, error) { <-entered; close(staged); return "ready", nil }
	r.activateRelease = func(context.Context, string) error {
		activated <- struct{}{}
		cancel()
		return fmt.Errorf("test stops before exec")
	}
	done := make(chan error, 1)
	go func() { done <- r.Daemon(ctx) }()
	select {
	case <-staged:
	case <-time.After(5 * time.Second):
		t.Fatal("update not staged")
	}
	select {
	case <-activated:
		t.Fatal("switched while work active")
	case <-time.After(1200 * time.Millisecond):
	}
	if beats.Load() < 2 {
		t.Fatal("heartbeats stopped during pending update")
	}
	close(finish)
	select {
	case <-activated:
	case <-time.After(5 * time.Second):
		t.Fatal("did not activate after work")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadBundleIntegrityAndStaging(t *testing.T) {
	old := Version
	Version = "v1.0.0"
	defer func() { Version = old }()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"emporetic-agent", ".agents/skills/emporetic-agent/SKILL.md", "examples/README.md", "examples/adapter.mjs", "examples/adapter-args.mjs", "examples/pi-executor.mjs", "examples/pi-picker.mjs", "examples/pi-judge.mjs"} {
		data := []byte("fixture")
		if name == "emporetic-agent" {
			data = []byte("#!/bin/sh\necho v1.1.0\n")
		}
		if err := tw.WriteHeader(&tar.Header{Name: "versions/v1.1.0/" + name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	payload := buf.Bytes()
	digest := sha256.Sum256(payload)
	url := "https://github.com/emporetic/agent-client/releases/download/v1.1.0/emporetic-agent_" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	for _, kind := range []string{"valid", "checksum", "truncated", "wrong-host", "http-error"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "versions"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("versions/v1.0.0", filepath.Join(root, "current")); err != nil {
				t.Fatal(err)
			}
			a := ReleaseAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: url, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload))}
			body := payload
			status := 200
			switch kind {
			case "checksum":
				a.SHA256 = strings.Repeat("0", 64)
			case "truncated":
				body = body[:len(body)-1]
			case "wrong-host":
				a.URL = "https://example.com/package.tar.gz"
			case "http-error":
				status = 503
			}
			h := &http.Client{Transport: updateTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "" {
					t.Fatal("credentials leaked to GitHub")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			path, err := stageBundle(context.Background(), root, Release{Version: "v1.1.0", Assets: []ReleaseAsset{a}}, h)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if path != filepath.Join(root, "versions", "v1.1.0") {
					t.Fatal(path)
				}
			} else if err == nil {
				t.Fatal("invalid download accepted")
			}
			pointer, err := os.Readlink(filepath.Join(root, "current"))
			if err != nil || pointer != "versions/v1.0.0" {
				t.Fatal("download switched running version")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".download-") {
					t.Fatal("download leaked")
				}
			}
		})
	}
}

func TestHeartbeatIgnoresMalformedOptionalRelease(t *testing.T) {
	a := NewAPI("https://example.com", "test")
	a.HTTP = &http.Client{Transport: updateTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"seq":1,"client_update":"invalid"}`)), Header: make(http.Header)}, nil
	})}
	release, err := a.heartbeatRelease(context.Background(), 1)
	if err != nil || release != nil {
		t.Fatalf("optional metadata affected heartbeat: %+v %v", release, err)
	}
}

// Release CI invokes this against its native package after building both targets.
func TestBuiltReleaseArchive(t *testing.T) {
	archive := os.Getenv("EMPORETIC_TEST_ARCHIVE")
	if archive == "" {
		t.Skip("set EMPORETIC_TEST_ARCHIVE after building a native release package")
	}
	version := os.Getenv("EMPORETIC_TEST_VERSION")
	if !stableVersion.MatchString(version) {
		t.Fatal("a stable test version is required")
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dest := t.TempDir()
	if err := extractBundle(f, dest, version); err != nil {
		t.Fatal(err)
	}
	if err := checkBundle(context.Background(), dest, version); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, ".agents/skills/emporetic-agent/scripts/client.sh"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("skill launcher must remain executable", err)
	}
}
