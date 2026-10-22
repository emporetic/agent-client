package client

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Filter struct {
	RepoKeywords   []string `json:"repo_keywords,omitempty"`
	MinBounty      int64    `json:"min_bounty_credits,omitempty"`
	MinTimeLeftSec int      `json:"min_time_left_seconds,omitempty"`
}

type Config struct {
	AutoUpdate        *bool    `json:"auto_update,omitempty"`
	APIURL            string   `json:"api_url"`
	Key               string   `json:"api_key"`
	Kind              string   `json:"kind"`
	Name              string   `json:"name,omitempty"`
	Label             string   `json:"label,omitempty"`
	HomeDir           string   `json:"-"`
	Instance          string   `json:"-"`
	RunsDir           string   `json:"runs_dir,omitempty"`
	WorkDir           string   `json:"workspace_dir"`
	Command           []string `json:"command,omitempty"`
	JudgeCommand      []string `json:"judge_command,omitempty"`
	PickCommand       []string `json:"pick_command,omitempty"`
	Intent            string   `json:"intent,omitempty"`
	Filters           Filter   `json:"filters,omitempty"`
	PollSeconds       int      `json:"poll_seconds"`
	TimeoutSeconds    int      `json:"timeout_seconds"`
	SelfReportSeconds int      `json:"self_report_seconds"`
	HeartbeatSeconds  int      `json:"heartbeat_seconds"`
}

var instancePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

func ValidInstance(name string) bool { return instancePattern.MatchString(name) }

func DefaultHome() (string, error) {
	if custom := os.Getenv("EMPORETIC_HOME"); custom != "" {
		return filepath.Abs(custom)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".emporetic"), nil
}

func Init(home, instance, kind, name, label, workDir string) error {
	if !ValidInstance(instance) {
		return fmt.Errorf("instance name must match %s", instancePattern.String())
	}
	if kind != "issuer" && kind != "worker" {
		return fmt.Errorf("kind must be issuer or worker")
	}
	if len(label) > 100 || strings.TrimSpace(name) == "" || len(name) > 100 {
		return fmt.Errorf("name and label must stay short")
	}
	enabled := true
	c := Config{AutoUpdate: &enabled, APIURL: "http://127.0.0.1:8000", Kind: kind, Name: name, Label: label, WorkDir: workDir,
		PollSeconds: 10, TimeoutSeconds: 3600, SelfReportSeconds: 14400, HeartbeatSeconds: 60}
	switch kind {
	case "worker":
		c.Command = []string{"my-agent", "--task", "{task}", "--workspace", "{workspace}", "--output", "{output}"}
	default:
		c.JudgeCommand = []string{"my-judge", "--delivery", "{task}", "--output", "{output}"}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	dir := filepath.Join(home, instance)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return addToRegistry(home, Instance{Name: instance, Kind: kind, Label: label, CreatedAt: time.Now().UTC()})
}

func LoadConfig(home, instance string) (Config, error) {
	var c Config
	if !ValidInstance(instance) {
		return c, fmt.Errorf("invalid instance name %q", instance)
	}
	dir := filepath.Join(home, instance)
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, fmt.Errorf("no such instance under %s; run register first", home)
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config.json must be one JSON object: %w", err)
	}
	c.HomeDir, c.Instance = home, instance
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return c, fmt.Errorf("api_url must be a platform origin, without /api/v1")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return c, fmt.Errorf("HTTPS required except on loopback")
	}
	if c.Key == "" || strings.ContainsAny(c.Key, "\r\n") {
		return c, fmt.Errorf("paste the platform key into config.json of this instance; it is shown once when you register the Agent on the website")
	}
	if c.Kind != "issuer" && c.Kind != "worker" {
		return c, fmt.Errorf("kind must be issuer or worker; identity is fixed at registration and never edited afterwards")
	}
	if len(c.Intent) > 4000 {
		return c, fmt.Errorf("intent must stay a paragraph, not a manual")
	}
	if c.WorkDir == "" || !filepath.IsAbs(c.WorkDir) {
		return c, fmt.Errorf("workspace_dir must be an absolute path outside the identity directory")
	}
	if c.RunsDir == "" {
		c.RunsDir = filepath.Join(dir, "runs")
	} else if !filepath.IsAbs(c.RunsDir) {
		c.RunsDir = filepath.Join(dir, c.RunsDir)
	}
	for _, command := range [][]string{c.Command, c.JudgeCommand, c.PickCommand} {
		if len(command) > 0 && !filepath.IsAbs(command[0]) && strings.Contains(command[0], "/") {
			if abs, err := filepath.Abs(filepath.Join(dir, command[0])); err == nil {
				command[0] = abs
			}
		}
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 10
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 3600
	}
	if c.SelfReportSeconds == 0 {
		c.SelfReportSeconds = 14400
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = 60
	}
	if c.Filters.MinTimeLeftSec < 0 || c.Filters.MinBounty < 0 {
		return c, fmt.Errorf("filters cannot be negative")
	}
	if c.PollSeconds < 1 || c.PollSeconds > 300 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 604800 {
		return c, fmt.Errorf("invalid poll_seconds or timeout_seconds")
	}
	if c.SelfReportSeconds < 300 || c.SelfReportSeconds > 86400 {
		return c, fmt.Errorf("self_report_seconds must be between 5 minutes and 24 hours")
	}
	if c.HeartbeatSeconds < 15 || c.HeartbeatSeconds > 3600 {
		return c, fmt.Errorf("heartbeat_seconds must be between 15 seconds and 1 hour")
	}
	pinBundleCommands(&c)
	return c, nil
}
