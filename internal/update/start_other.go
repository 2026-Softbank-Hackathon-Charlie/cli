//go:build !windows

package update

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached runs exe in its own session, so it outlives the command that
// started it and a terminal hangup or Ctrl-C does not reach it.
func startDetached(exe string, args []string, logPath string) error {
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func hideWindow(*exec.Cmd) {}
