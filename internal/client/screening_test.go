package client

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run an actual child process so failures and missing artifacts exercise the
// same path as a configured picker adapter.
func TestScreeningProcessHelper(t *testing.T) {
	if os.Getenv("EMPORETIC_TEST_PICKER") != "1" {
		return
	}
	mode, output := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if mode == "fail" {
		os.Exit(4)
	}
	if mode == "no-output" {
		os.Exit(0)
	}
	if err := os.WriteFile(filepath.Join(output, "picks.json"), []byte(mode), 0600); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func screeningRunner(t *testing.T, result string) *Runner {
	t.Helper()
	t.Setenv("EMPORETIC_TEST_PICKER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{
		Config: Config{
			RunsDir:        t.TempDir(),
			TimeoutSeconds: 10,
			PickCommand:    []string{executable, "-test.run=^TestScreeningProcessHelper$", "--", "{screening}", result, "{output}"},
		},
		API: &API{},
		Out: io.Discard,
	}
}

func TestConfiguredScreeningRequiresAnExplicitPick(t *testing.T) {
	for _, test := range []struct {
		name   string
		result string
		reason string
	}{
		{"process failure", "fail", "title screening failed"},
		{"empty ids", `{"ids":[]}`, "skipped every candidate"},
		{"missing ids", `{"skipped":[]}`, "skipped every candidate"},
		{"null ids", `{"ids":null}`, "skipped every candidate"},
		{"null result", `null`, "skipped every candidate"},
		{"missing artifact", "no-output", "must write output/picks.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := screeningRunner(t, test.result)
			next, reason := r.chooseTask([]Task{{ID: "tsk_first"}, {ID: "tsk_second"}})
			if next != nil || !strings.Contains(reason, test.reason) {
				t.Fatalf("screening accepted unselected work: task=%+v reason=%q", next, reason)
			}
		})
	}
}

func TestScreeningDoesNotReuseAnEarlierResult(t *testing.T) {
	r := screeningRunner(t, `{"ids":["tsk_first"]}`)
	tasks := []Task{{ID: "tsk_first"}}
	if next, reason := r.chooseTask(tasks); next == nil || next.ID != tasks[0].ID || reason != "" {
		t.Fatalf("first screening did not select the candidate: task=%+v reason=%q", next, reason)
	}
	r.Config.PickCommand[len(r.Config.PickCommand)-2] = "no-output"
	if next, reason := r.chooseTask(tasks); next != nil || !strings.Contains(reason, "must write output/picks.json") {
		t.Fatalf("reused a stale screening result: task=%+v reason=%q", next, reason)
	}
}

func TestScreeningWithoutACommandKeepsFirstCandidate(t *testing.T) {
	r := &Runner{Out: io.Discard}
	next, reason := r.chooseTask([]Task{{ID: "tsk_first"}, {ID: "tsk_second"}})
	if next == nil || next.ID != "tsk_first" || reason != "" {
		t.Fatalf("unconfigured screening changed selection: task=%+v reason=%q", next, reason)
	}
}

func TestScreeningWithAnEmptyExecutableDoesNotPick(t *testing.T) {
	r := &Runner{Config: Config{PickCommand: []string{""}}, Out: io.Discard}
	if next, reason := r.chooseTask([]Task{{ID: "tsk_first"}}); next != nil || !strings.Contains(reason, "configure a screening executable") {
		t.Fatalf("invalid configured screening selected work: task=%+v reason=%q", next, reason)
	}
}
