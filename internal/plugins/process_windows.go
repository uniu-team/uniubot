//go:build windows

package plugins

import (
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

func signalProcess(pid int, sig os.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(sig)
}
