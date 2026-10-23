//go:build unix

package sources

import (
	"os/exec"
	"syscall"
)

// inGroup starts cmd in a process group of its own, and has canceling it
// stop the whole group. yt-dlp runs Bun as a child, and its packed Linux
// build runs Python as one, so stopping yt-dlp alone would leave them going.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// stopGroup stops what's left of cmd's process group once cmd has finished,
// like a child it left running when it crashed.
func stopGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
