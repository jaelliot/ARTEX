//go:build linux

package server

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureScriptProcess terminates the script's process group on cancellation.
func configureScriptProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	c.Cancel = stop
}
