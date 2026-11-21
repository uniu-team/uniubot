//go:build !windows

package devserver

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup isolates Vite and the npm wrapper from the host group.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcessGroup(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	converted, ok := sig.(syscall.Signal)
	if !ok {
		return cmd.Process.Signal(sig)
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, converted); err != nil {
		return cmd.Process.Signal(sig)
	}
	return nil
}
