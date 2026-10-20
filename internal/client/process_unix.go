//go:build linux || darwin

package client

import (
	"os/exec"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd) (func(), error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	cmd.Cancel = func() error { kill(); return nil }
	return kill, nil
}
func attachProcess(_ *exec.Cmd) error { return nil }
