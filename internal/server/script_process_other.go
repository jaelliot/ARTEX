//go:build !linux

package server

import "os/exec"

func configureScriptProcess(_ *exec.Cmd) {}
