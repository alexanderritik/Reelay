package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexanderritik/reelay/internal/storage"
)

// Fault injection for cmd/chaos, selected with CONDUIT_CHAOS. Off by default.
// These faults must happen at an exact point in the job, which a signal sent
// from outside the process cannot hit reliably.
const (
	// ChaosFreezeBeforeCommit stops the whole process (SIGSTOP to itself) just
	// before the commit, like a long GC or VM pause. Every goroutine, the
	// heartbeat included, freezes until someone sends SIGCONT.
	ChaosFreezeBeforeCommit = "freeze-before-commit"

	// ChaosTruncateProxy cuts the stored proxy to 60% before validation, like
	// a full disk or an interrupted upload.
	ChaosTruncateProxy = "truncate-proxy"

	// ChaosCrashDuringPlan kills the worker (SIGKILL to itself) after it has
	// split a long source into chunks but before the plan is written.
	ChaosCrashDuringPlan = "crash-during-plan"

	// ChaosCrashDuringFinalize kills the worker after it has moved a chunked
	// job to validating but before it has checked the seams.
	ChaosCrashDuringFinalize = "crash-during-finalize"
)

func ValidChaos(mode string) bool {
	switch mode {
	case "", ChaosFreezeBeforeCommit, ChaosTruncateProxy, ChaosCrashDuringPlan, ChaosCrashDuringFinalize:
		return true
	}
	return false
}

// chaosCrash kills the worker outright if mode is the active fault: no
// deferred cleanup, no final write, like a power cut or the OOM killer.
func (w *Worker) chaosCrash(log *slog.Logger, mode string) {
	if w.Chaos != mode {
		return
	}
	log.Warn("chaos: crashing", "mode", mode, "pid", os.Getpid())
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // SIGKILL cannot be caught; never get past this point
}

func (w *Worker) chaosBeforeCommit(log *slog.Logger) {
	if w.Chaos != ChaosFreezeBeforeCommit {
		return
	}
	log.Warn("chaos: freezing before commit", "pid", os.Getpid())
	// kill(self, SIGSTOP) can return before the stop takes effect, which
	// would let the commit start and then freeze halfway through it. Waiting
	// for the SIGCONT that ends the stop pins the freeze to this point.
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	<-cont
	log.Warn("chaos: resumed")
}

func (w *Worker) chaosAfterStore(ctx context.Context, log *slog.Logger, key, dir string) error {
	if w.Chaos != ChaosTruncateProxy {
		return nil
	}
	path, err := storage.LocalFile(ctx, w.Storage, key, dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	log.Warn("chaos: truncating stored proxy", "key", key, "from", info.Size(), "to", info.Size()*6/10)
	if err := os.Truncate(path, info.Size()*6/10); err != nil {
		return fmt.Errorf("chaos truncate: %w", err)
	}
	return nil
}
