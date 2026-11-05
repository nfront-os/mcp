//go:build !plan9

package main

// For testing on Unix: the command in a process group of its own, killed as
// a whole.

import (
	"os/exec"
	"strconv"
	"syscall"
)

const defaultShell = "rc"

func newGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// diskGuard: only Plan 9 has driver masks; elsewhere the disks are as the
// user's rights leave them.
func diskGuard() (string, []string, error) {
	return "", nil, nil
}

func exitStatus(ee *exec.ExitError) string {
	return "exit " + strconv.Itoa(ee.ExitCode())
}
