//go:build !windows

package plugins

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup isolates the process group so helper processes (conda,
// interpreters) die together with the plugin.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcess(pid int, sig os.Signal) error {
	converted, ok := sig.(syscall.Signal)
	if !ok {
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	if err := syscall.Kill(-pid, converted); err != nil {
		return syscall.Kill(pid, converted)
	}
	return nil
}
