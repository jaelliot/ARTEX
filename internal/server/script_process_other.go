//go:build !linux

package server

import "os/exec"

func runScriptProcess(c *exec.Cmd) error { return c.Run() }
