//go:build linux

package server

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// runScriptProcess kills remaining group members before reaping the leader.
// Keeping the leader waitable prevents its PID/PGID from being reused during
// cleanup. The mutex also prevents a late context cancellation from signaling
// the group after Wait has reaped the leader.
func runScriptProcess(c *exec.Cmd) error {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var mu sync.Mutex
	active := true
	stop := func() error {
		if !active || c.Process == nil {
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
	c.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		return stop()
	}
	if err := c.Start(); err != nil {
		return err
	}
	var info unix.Siginfo
	var waitErr error
	for {
		waitErr = unix.Waitid(unix.P_PID, c.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(waitErr, syscall.EINTR) {
			break
		}
	}
	mu.Lock()
	var stopErr error
	if !errors.Is(waitErr, syscall.ECHILD) {
		stopErr = stop()
		if errors.Is(stopErr, os.ErrProcessDone) {
			stopErr = nil
		}
	}
	active = false
	mu.Unlock()
	return errors.Join(waitErr, stopErr, c.Wait())
}
