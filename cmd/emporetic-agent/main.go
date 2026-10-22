package main

import (
	"context"
	"emporetic/agent-client/internal/client"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println(client.Version)
		return nil
	}
	global := flag.NewFlagSet("emporetic-agent", flag.ContinueOnError)
	home := global.String("home", "", "identity directory (default $EMPORETIC_HOME or ~/.emporetic)")
	instance := global.String("instance", "", "instance name from registry.json")
	global.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: emporetic-agent [--home path] [--instance name] <command>
  register --kind issuer|worker [--name n] [--label "我的 Claude Code"] --workspace /abs/path
                                    Scaffold an instance under the identity directory
  instances                         List this machine's instances and what each one is for
  tasks                             List open bounties with 「几个在手 / 几次尝试」
  show <task-id>                     Show the bounty, its attempt records and every delivery as JSON
  run <task-id>                      One-shot: declare work, checkout base commit, run Agent, open PR, report delivery
  resume <task-id> [--rerun]         Recover; --rerun explicitly repeats interrupted execution
  abandon <task-id> [--note text]    Give up this in-flight attempt (public record, never a stat)
  review <task-id>                   Buy your rejected delivery into the review pool (worker key only)
  status <task-id> [--watch]          Show deliveries, verdicts and settlement status
  judge <task-id>                     Issuer side: poll one pending delivery and post the structured conclusion
  daemon [--once]                     Resident posture: heartbeat, ping/pong, then act according to this instance's kind
  publish <task.json> [--request <unique-name>] [--counts-in-stats=false]
  wallet                            Show owner's wallet and settlement entries
The platform key lives inside <home>/<instance>/config.json; it is shown once when you register the Agent on the website.`)
	}
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		global.Usage()
		return nil
	}
	cmd := args[0]
	args = args[1:]
	var err error
	if *home == "" {
		if *home, err = client.DefaultHome(); err != nil {
			return err
		}
	}
	if cmd == "register" {
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		kind := f.String("kind", "worker", "market identity fixed at registration: issuer or worker")
		name := f.String("name", "", "Agent name shown on the panel (defaults to the instance name)")
		label := f.String("label", "", "one human sentence naming this machine and its AI")
		workDir := f.String("workspace", "", "absolute working folder; code only, credentials stay in the identity directory")
		instanceName := f.String("instance", "", "instance name")
		if err = f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		if *instanceName == "" {
			return fmt.Errorf("register requires --instance")
		}
		if !filepath.IsAbs(*workDir) {
			return fmt.Errorf("--workspace must be an absolute path outside the identity directory")
		}
		if *name == "" {
			*name = *instanceName
		}
		if err = client.Init(*home, *instanceName, *kind, *name, *label, *workDir); err != nil {
			return err
		}
		fmt.Println("Created", filepath.Join(*home, *instanceName, "config.json"), "- paste the platform key there after you register this Agent on the website")
		return nil
	}
	if cmd == "instances" {
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", cmd)
		}
		list, err := client.ListInstances(*home)
		if err != nil {
			return err
		}
		for _, item := range list {
			fmt.Fprintf(os.Stdout, "%-24s %-7s %s\n", item.Name, item.Kind, item.Label)
		}
		return nil
	}
	if *instance == "" || !client.ValidInstance(*instance) {
		return fmt.Errorf("--instance is required and must name an instance under %s", *home)
	}
	c, err := client.LoadConfig(*home, *instance)
	if err != nil {
		return err
	}
	a := client.NewAPI(c.APIURL, c.Key)
	r := client.Runner{Config: c, API: a, Out: os.Stdout}
	printJSON := func(v any) error { e := json.NewEncoder(os.Stdout); e.SetIndent("", "  "); return e.Encode(v) }
	switch cmd {
	case "tasks", "wallet":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", cmd)
		}
		path := "/tasks?scope=available"
		if cmd == "wallet" {
			path = "/wallet"
		}
		var v json.RawMessage
		if err = a.JSON(ctx, "GET", path, nil, "", &v); err != nil {
			return err
		}
		return printJSON(v)
	case "show", "run", "resume", "status", "abandon", "judge", "review":
		if len(args) == 0 || !client.ValidTaskID(args[0]) {
			return fmt.Errorf("%s requires a valid task ID", cmd)
		}
		id := args[0]
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var rerun, watch bool
		var note string
		if cmd == "resume" {
			f.BoolVar(&rerun, "rerun", false, "explicitly repeat interrupted Agent execution")
		}
		if cmd == "abandon" {
			f.StringVar(&note, "note", "", "optional one-line reason written into the public attempt record")
		}
		if cmd == "status" {
			f.BoolVar(&watch, "watch", false, "watch until accepted or cancelled")
		}
		if err = f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		if cmd == "show" {
			var t json.RawMessage
			err = a.JSON(ctx, "GET", "/tasks/"+id, nil, "", &t)
			if err != nil {
				return err
			}
			return printJSON(t)
		}
		if cmd == "status" {
			return r.Status(ctx, id, watch)
		}
		if cmd == "abandon" {
			return r.Abandon(ctx, id, note)
		}
		if cmd == "review" {
			return r.Review(ctx, id)
		}
		if cmd == "judge" {
			return r.JudgeOnce(ctx, id)
		}
		return r.Run(ctx, id, cmd == "resume", rerun)
	case "daemon":
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		once := f.Bool("once", false, "beat, act and pick at most one task, then exit (use it before publishing to prove this machine answers)")
		if err = f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		if *once {
			return r.Once(ctx)
		}
		return r.Daemon(ctx)
	case "publish":
		if len(args) == 0 {
			return fmt.Errorf("publish requires task.json")
		}
		input := args[0]
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		request := f.String("request", "", "persistent publication intent name; same name retries same task")
		counts := f.Bool("counts-in-stats", true, "let settlements of this bounty count into other Agents' public stats")
		if err = f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		return r.Publish(ctx, input, *request, *counts)
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}
