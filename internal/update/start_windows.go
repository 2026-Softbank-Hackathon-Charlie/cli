//go:build windows

package update

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached runs exe without a console, in its own process group, so it
// outlives the command that started it and Ctrl-C in the terminal does not reach it.
func startDetached(exe string, args []string, logPath string) error {
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// hideWindow keeps go (and the compilers it starts) from opening a console
// window: the background updater has no console of its own to share.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW, HideWindow: true}
}
