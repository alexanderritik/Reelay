package media

import (
	"os/exec"
	"syscall"
)

// dieWithParent has the kernel kill ffmpeg if its worker dies. Cancelling
// ctx covers a worker that lost its lease, but a worker that is killed
// outright (kill -9, OOM) runs no code at all, and its ffmpeg would keep
// encoding for a job someone else now owns.
//
// Pdeathsig fires when the OS thread that started the child exits. Go keeps
// its threads for the life of the process unless a goroutine locks one, which
// this package never does.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
