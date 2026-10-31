package worker

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexanderritik/reelay/internal/jobs"
)

// Progress is written at most once per progressInterval and only when it has
// moved by progressMinStep, so one attempt costs at most ~100 writes however
// long it runs. The interval is short because chunks finish in a second or
// two; at one write a second they jumped from 0% straight to done.
const (
	progressInterval = 250 * time.Millisecond
	progressMinStep  = 1.0 // percentage points
	// progressCap keeps a running job below 100: it reaches 100 only when the
	// fenced commit lands, never while the proxy could still fail validation.
	progressCap = 99.0
)

// reportProgress returns a callback for ffmpeg's progress and a stop
// function. The callback only records the latest value: it runs inside the
// loop reading ffmpeg's output, so it must never wait on the database. A
// separate goroutine writes the value at most once per progressInterval, and
// only when it moved by progressMinStep.
func (w *Worker) reportProgress(ctx context.Context, log *slog.Logger, job jobs.Job, total time.Duration) (update func(time.Duration), stop func()) {
	var latest atomic.Uint64 // math.Float64bits of the percentage
	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(progressInterval)
		defer tick.Stop()
		written := 0.0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			p := math.Float64frombits(latest.Load())
			if p-written < progressMinStep {
				continue
			}
			callCtx, callCancel := context.WithTimeout(ctx, time.Second)
			err := w.Jobs.SetProgress(callCtx, job.ID, job.Fence, p)
			callCancel()
			switch {
			case err == nil:
				written = p
			case errors.Is(err, jobs.ErrLeaseLost), ctx.Err() != nil:
				// The heartbeat notices a lost lease and stops the job.
			default:
				log.Debug("progress update failed", "err", err)
			}
		}
	})

	update = func(encoded time.Duration) {
		if total <= 0 {
			return // unknown duration: no percentage to report
		}
		p := min(progressCap, 100*encoded.Seconds()/total.Seconds())
		latest.Store(math.Float64bits(math.Round(p*10) / 10))
	}
	stop = func() {
		cancel()
		wg.Wait()
	}
	return update, stop
}
