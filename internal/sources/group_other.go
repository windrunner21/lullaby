//go:build !unix

package sources

import "os/exec"

// inGroup does nothing here: canceling cmd stops yt-dlp itself.
func inGroup(*exec.Cmd) {}

// stopGroup does nothing here.
func stopGroup(*exec.Cmd) {}
