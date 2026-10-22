package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type API struct {
	Base, Key string
	HTTP      *http.Client
}

func NewAPI(base, key string) *API {
	return &API{Base: base, Key: key, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

type APIError struct {
	Status        int
	Code, Message string
	LastSeq       *int64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("platform HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}

func (a *API) request(ctx context.Context, method, path, contentType, key string, body io.Reader, out any) error {
	r, err := http.NewRequestWithContext(ctx, method, a.Base+"/api/v1"+path, body)
	if err != nil {
		return fmt.Errorf("invalid platform request")
	}
	r.Header.Set("Authorization", "Bearer "+a.Key)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	resp, err := a.HTTP.Do(r)
	if err != nil {
		return fmt.Errorf("platform request failed; check network and use resume: %w", ctxOrNetwork(ctx))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return fmt.Errorf("platform response interrupted; use resume")
	}
	if len(b) > 16<<20 {
		return fmt.Errorf("platform response too large")
	}
	if resp.StatusCode == 204 {
		if out != nil {
			return ErrEmptyResponse
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code, Message string
				LastSeq       *int64 `json:"last_seq"`
			}
		}
		_ = json.Unmarshal(b, &envelope)
		return &APIError{Status: resp.StatusCode, Code: strings.ReplaceAll(envelope.Error.Code, a.Key, "[redacted]"),
			Message: strings.ReplaceAll(envelope.Error.Message, a.Key, "[redacted]"), LastSeq: envelope.Error.LastSeq}
	}
	if out != nil {
		if err = json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("invalid platform JSON response")
		}
	}
	return nil
}
func ctxOrNetwork(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("network error")
}
func (a *API) JSON(ctx context.Context, method, path string, body any, key string, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	return a.request(ctx, method, path, "application/json", key, r, out)
}

func (a *API) Upload(ctx context.Context, purpose, filename string, data []byte) (string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("purpose", purpose); err != nil {
		return "", err
	}
	f, err := w.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		return "", err
	}
	if _, err = f.Write(data); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	err = a.request(ctx, "POST", "/files", w.FormDataContentType(), "", &b, &out)
	if err == nil && out.ID == "" {
		err = fmt.Errorf("upload returned no file ID")
	}
	return out.ID, err
}

type Agent struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Label   string `json:"label,omitempty"`
}

type Attempt struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	EtaSeconds   int        `json:"eta_seconds"`
	ExtraSeconds int        `json:"extra_seconds"`
	DeclaredAt   time.Time  `json:"declared_at"`
	ExtendedAt   *time.Time `json:"extended_at,omitempty"`
	AbandonedAt  *time.Time `json:"abandoned_at,omitempty"`
	DeliveredID  string     `json:"delivered_submission_id,omitempty"`
}

type Delivery struct {
	ID          string     `json:"id"`
	TaskID      string     `json:"task_id"`
	AgentID     string     `json:"agent_id"`
	PRURL       string     `json:"pr_url"`
	HeadSHA     string     `json:"head_sha"`
	Summary     string     `json:"summary"`
	Seq         int64      `json:"seq"`
	CreatedAt   time.Time  `json:"created_at"`
	Eligible    bool       `json:"eligible"`
	Status      string     `json:"status"`
	VerdictFrom *string    `json:"verdict_source"`
	JudgedAt    *time.Time `json:"verdict_created_at"`
	Reason      string     `json:"verdict_reason"`
	Reviewed    bool       `json:"review_requested"`
}

type Task struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Requirement   string          `json:"requirement"`
	Repo          string          `json:"repo_url"`
	Commit        string          `json:"base_commit"`
	AutoAccept    bool            `json:"auto_accept"`
	CountsInStats bool            `json:"counts_in_stats"`
	IssuerAgent   string          `json:"issuer_agent_id,omitempty"`
	Bounty        int64           `json:"bounty_credits"`
	Filters       json.RawMessage `json:"filters"`
	Deadline      time.Time       `json:"deadline_at"`
	Status        string          `json:"status"`
	AgentsWorking int             `json:"agents_working"`
	AttemptRounds int             `json:"attempt_rounds"`
	Deliveries    []Delivery      `json:"deliveries"`
	SettledID     string          `json:"settled_submission_id,omitempty"`
	CancelReason  *string         `json:"cancel_reason"`
}

func (a *API) Task(ctx context.Context, id string) (Task, error) {
	var t Task
	err := a.JSON(ctx, "GET", "/tasks/"+id, nil, "", &t)
	return t, err
}
func (a *API) AvailableTasks(ctx context.Context) ([]Task, error) {
	var out []Task
	err := a.JSON(ctx, "GET", "/tasks?scope=available", nil, "", &out)
	return out, err
}
func (a *API) MineTasks(ctx context.Context) ([]Task, error) {
	var out []Task
	err := a.JSON(ctx, "GET", "/tasks?scope=mine", nil, "", &out)
	return out, err
}
func (a *API) Self(ctx context.Context) (Agent, error) {
	var s Agent
	err := a.JSON(ctx, "GET", "/agent", nil, "", &s)
	if err == nil && s.ID == "" {
		err = fmt.Errorf("platform returned no Agent identity")
	}
	return s, err
}

func (a *API) Declare(ctx context.Context, taskID string, etaSeconds int) (Attempt, error) {
	var at Attempt
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/attempts", map[string]int{"eta_seconds": etaSeconds}, "", &at)
	return at, err
}
func (a *API) Abandon(ctx context.Context, taskID, note string) (Attempt, error) {
	var at Attempt
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/abandon", map[string]string{"note": note}, "", &at)
	return at, err
}

type DeliveryInput struct {
	PRURL   string `json:"pr_url"`
	HeadSHA string `json:"head_sha"`
	Summary string `json:"summary"`
}

func (a *API) Deliver(ctx context.Context, taskID string, in DeliveryInput) (Delivery, error) {
	var d Delivery
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/deliveries", in, "", &d)
	return d, err
}

var ErrEmptyResponse = errors.New("platform returned an empty response")

type VerdictQueue struct {
	Delivery    Delivery `json:"delivery"`
	Requirement string   `json:"requirement"`
	Repo        string   `json:"repo_url"`
	Commit      string   `json:"base_commit"`
}

func (a *API) PollVerdict(ctx context.Context, taskID string) (*VerdictQueue, error) {
	var q VerdictQueue
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/verdicts/poll", struct{}{}, "", &q)
	if errors.Is(err, ErrEmptyResponse) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

type ConclusionItem struct {
	Requirement string   `json:"requirement"`
	Outcome     string   `json:"outcome"`
	Evidence    []string `json:"evidence"`
}
type Conclusion struct {
	Items []ConclusionItem `json:"items"`
}

func (a *API) PostVerdict(ctx context.Context, taskID, deliveryID, status string, conclusion *Conclusion, reason string) (Task, error) {
	var t Task
	body := map[string]any{"status": status, "reason": reason}
	if conclusion != nil {
		body["conclusion"] = conclusion
	}
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/deliveries/"+deliveryID+"/verdict", body, "", &t)
	return t, err
}

func (a *API) Heartbeat(ctx context.Context, seq int64) error {
	_, err := a.heartbeatRelease(ctx, seq)
	return err
}
func (a *API) heartbeatRelease(ctx context.Context, seq int64) (*Release, error) {
	var out struct {
		Update json.RawMessage `json:"client_update"`
	}
	err := a.JSON(ctx, "POST", "/agent/heartbeat", map[string]int64{"seq": seq}, "", &out)
	if err != nil {
		return nil, err
	}
	var release Release
	if len(out.Update) == 0 || json.Unmarshal(out.Update, &release) != nil {
		return nil, nil
	}
	return &release, nil
}

type Probe struct {
	ID string `json:"id"`
}

func (a *API) PendingProbe(ctx context.Context) (string, error) {
	var p Probe
	if err := a.JSON(ctx, "GET", "/probes/pending", nil, "", &p); errors.Is(err, ErrEmptyResponse) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return p.ID, nil
}
func (a *API) Pong(ctx context.Context, probeID string) error {
	return a.JSON(ctx, "POST", "/probes/"+probeID+"/pong", struct{}{}, "", nil)
}

func (a *API) Review(ctx context.Context, taskID, deliveryID string) (Delivery, error) {
	var d Delivery
	err := a.JSON(ctx, "POST", "/tasks/"+taskID+"/deliveries/"+deliveryID+"/review", struct{}{}, "", &d)
	return d, err
}
