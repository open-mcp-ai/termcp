//go:build !windows

package sshserver

import (
	"os/exec"
	"syscall"
)

func setPtySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
