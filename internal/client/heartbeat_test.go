package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func heartbeatRunner(dir, base string) *Runner {
	return &Runner{Config: Config{RunsDir: dir}, API: NewAPI(base, "test-key"), Out: io.Discard}
}

func TestHeartbeatReservesSequenceBeforeRequestAndSurvivesRestart(t *testing.T) {
	for _, failure := range []string{"lost response", "interrupted response", "service unavailable"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "heartbeat.json")
			if err := saveJSON(path, struct{ Seq int64 }{520}); err != nil {
				t.Fatal(err)
			}
			last, calls, probes := int64(520), 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/v1/probes/pending" {
					probes++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls++
				var in struct{ Seq int64 }
				if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
					t.Error(err)
				}
				if seq, err := readSeq(path); err != nil || seq != in.Seq {
					t.Errorf("sequence must be durable before sending: saved=%d sent=%d err=%v", seq, in.Seq, err)
				}
				if in.Seq <= last {
					http.Error(w, `{"error":{"code":"STALE_HEARTBEAT"}}`, http.StatusConflict)
					return
				}
				if calls == 1 && failure == "service unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				last = in.Seq // The platform committed, but the client may never see its reply.
				if calls == 1 {
					if failure == "lost response" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, "{")
					return
				}
				_, _ = fmt.Fprintf(w, `{"seq":%d}`, last)
			}))
			defer srv.Close()
			if err := heartbeatRunner(dir, srv.URL).Beat(context.Background()); err == nil {
				t.Fatal("lost/failed response must be reported")
			}
			if seq, err := readSeq(path); err != nil || seq != 521 {
				t.Fatalf("failed attempt must consume sequence 521: seq=%d err=%v", seq, err)
			}
			// A fresh Runner has no memory of the ambiguous request.
			if err := heartbeatRunner(dir, srv.URL).Beat(context.Background()); err != nil {
				t.Fatal("next heartbeat must recover after restart:", err)
			}
			if last != 522 || calls != 2 || probes != 1 {
				t.Fatalf("last=%d calls=%d probes=%d", last, calls, probes)
			}
		})
	}
}

func TestHeartbeatRecoversStaleLocalState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local int64
		last  int64
		hint  bool
	}{
		{"server sequence ahead of clock", 520, 3_000_000_000_000, true},
		{"legacy platform without sequence hint", 520, 525, false},
		{"missing local state", 0, 525, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.local != 0 {
				if err := saveJSON(filepath.Join(dir, "heartbeat.json"), struct{ Seq int64 }{tc.local}); err != nil {
					t.Fatal(err)
				}
			}
			calls, probes := 0, 0
			last := tc.last
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/v1/probes/pending" {
					probes++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls++
				var in struct{ Seq int64 }
				_ = json.NewDecoder(req.Body).Decode(&in)
				if in.Seq <= last {
					w.WriteHeader(http.StatusConflict)
					if tc.hint {
						_, _ = fmt.Fprintf(w, `{"error":{"code":"STALE_HEARTBEAT","last_seq":%d}}`, last)
					} else {
						_, _ = io.WriteString(w, `{"error":{"code":"STALE_HEARTBEAT"}}`)
					}
					return
				}
				last = in.Seq
				_, _ = fmt.Fprintf(w, `{"seq":%d}`, last)
			}))
			defer srv.Close()
			if err := heartbeatRunner(dir, srv.URL).Beat(context.Background()); err != nil {
				t.Fatal(err)
			}
			if last <= tc.last || (tc.hint && last != tc.last+1) || calls != 2 || probes != 1 {
				t.Fatalf("resync must retry once and answer probes: last=%d calls=%d probes=%d", last, calls, probes)
			}
			if seq, err := readSeq(filepath.Join(dir, "heartbeat.json")); err != nil || seq != last {
				t.Fatalf("resync not persisted: seq=%d last=%d err=%v", seq, last, err)
			}
		})
	}
}

func TestHeartbeatRetriesOnlyStaleAndBoundsRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, code        string
		status, wantCalls int
	}{
		{"stale", "STALE_HEARTBEAT", 409, 2},
		{"other conflict", "STATE_CONFLICT", 409, 1},
		{"unauthorized", "UNAUTHORIZED", 401, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"last_seq":100}}`, tc.code)
			}))
			defer srv.Close()
			if err := heartbeatRunner(t.TempDir(), srv.URL).Beat(context.Background()); err == nil {
				t.Fatal("failed heartbeat must remain an error")
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", calls, tc.wantCalls)
			}
		})
	}
}

func TestHeartbeatRefusesSequenceOverflow(t *testing.T) {
	dir := t.TempDir()
	if err := saveJSON(filepath.Join(dir, "heartbeat.json"), struct{ Seq int64 }{math.MaxInt64}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { calls++ }))
	defer srv.Close()
	if err := heartbeatRunner(dir, srv.URL).Beat(context.Background()); err == nil || calls != 0 {
		t.Fatalf("overflow must fail before sending: err=%v calls=%d", err, calls)
	}
}

func TestHeartbeatLocksSequenceAcrossRunners(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockFile(filepath.Join(dir, ".heartbeat.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { calls++ }))
	defer srv.Close()
	if err := heartbeatRunner(dir, srv.URL).Beat(context.Background()); err == nil || calls != 0 {
		t.Fatalf("another runner holds the sequence lock: err=%v calls=%d", err, calls)
	}
	if seq, err := readSeq(filepath.Join(dir, "heartbeat.json")); err != nil || seq != 0 {
		t.Fatalf("lock contention must not consume a sequence: seq=%d err=%v", seq, err)
	}
}
