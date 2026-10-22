package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func childEnv(key string, git bool) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "EMPORETIC_API_KEY" || (key != "" && strings.Contains(entry, key)) {
			continue
		}
		if git && strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	if git {
		env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	}
	return env
}

func runProcess(ctx context.Context, dir string, env []string, command []string, stdout io.Writer) error {
	return runProcessOutput(ctx, dir, env, command, stdout, io.Discard)
}

func runProcessOutput(ctx context.Context, dir string, env []string, command []string, stdout, stderr io.Writer) error {
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("configure an Agent command first")
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	cleanup, err := prepareProcess(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("could not start executable %q", command[0])
	}
	if err = attachProcess(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("executable %q failed (%s)", command[0], cmd.ProcessState.String())
	}
	return nil
}
