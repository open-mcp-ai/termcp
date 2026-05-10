//go:build windows

package sshserver

import "os/exec"

func setPtySysProcAttr(cmd *exec.Cmd) {}
