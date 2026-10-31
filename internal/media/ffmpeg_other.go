//go:build !linux

package media

import "os/exec"

// dieWithParent is Linux-only (Pdeathsig); elsewhere a killed worker's
// ffmpeg runs until it finishes or fails to write its progress.
func dieWithParent(*exec.Cmd) {}
