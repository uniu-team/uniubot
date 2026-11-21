//go:build windows

package devserver

import (
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
