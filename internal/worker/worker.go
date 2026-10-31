// Package worker runs the claim → process loop for one job at a time, keeps
// the job's lease alive while it works, and reaps leases abandoned by other
// workers. Scale out by running more worker processes.
package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
	"github.com/alexanderritik/reelay/internal/storage"
)

// errLeaseExpired means renewals kept failing (e.g. the database is down)
// until the lease must have run out. The worker stops on its own rather than
// keep working on a job someone else may now own.
var errLeaseExpired = errors.New("lease expired before it could be renewed")

type Worker struct {
	ID           string
	Jobs         *jobs.Store
	LeaseTTL     time.Duration
	PollInterval time.Duration
	ReapInterval time.Duration
	Storage      storage.Storage
	WorkDir      string // scratch space for sources and proxies in progress
	Proxy        media.ProxySpec
	Chaos        string // fault injection for cmd/chaos; see chaos.go
	Log          *slog.Logger

	// Chunked path (chunked.go): sources at least ChunkMinDuration long are
	// split into chunks of ChunkSeconds, a multiple of Proxy.KeyframeInterval.
	ChunkSeconds     time.Duration
	ChunkMinDuration time.Duration
}

// NewID returns a worker ID that is unique across restarts, so a restarted
// worker never mistakes an old lease for its own.
func NewID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), rand.Text()[:6])
}

// Run claims and processes jobs until ctx is cancelled, with the reaper
// running alongside. Database errors are logged and retried rather than
// returned, so the worker rides out a Postgres restart.
func (w *Worker) Run(ctx context.Context) error {
	w.Log.Info("worker started", "worker", w.ID, "lease", w.LeaseTTL)

	var wg sync.WaitGroup
	wg.Go(func() { w.reapLoop(ctx) })
	defer wg.Wait()

	for {
		// Taken before the claim is sent, so it is no later than the database's
		// now() inside the claim: a local deadline based on it errs early.
		start := time.Now()
		job, err := w.Jobs.Claim(ctx, w.ID, w.LeaseTTL)
		switch {
		case err == nil:
			w.Log.Info("claimed job", "job", job.ID, "fence", job.Fence,
				"attempt", job.Attempts, "max_attempts", job.MaxAttempts)
			w.runJob(ctx, job, start.Add(w.LeaseTTL))
			continue // look for more work straight away
		case ctx.Err() != nil:
			w.Log.Info("worker stopped", "worker", w.ID)
			return nil
		case !errors.Is(err, jobs.ErrNoJob):
			w.Log.Error("claim failed", "err", err)
		}

		select {
		case <-ctx.Done():
			w.Log.Info("worker stopped", "worker", w.ID)
			return nil
		case <-time.After(w.PollInterval):
		}
	}
}

// runJob processes one job under a context that is cancelled if the lease is
// lost, so the work stops as soon as the worker learns it is no longer the
// owner. The heartbeat keeps running through finish, so the lease stays
// alive while the outcome is written.
func (w *Worker) runJob(ctx context.Context, job jobs.Job, leaseDeadline time.Time) {
	log := w.Log.With("job", job.ID, "fence", job.Fence)
	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	wg.Go(func() { w.heartbeat(jobCtx, cancel, job, leaseDeadline) })

	res, err := w.handle(jobCtx, log, job)
	w.finish(ctx, jobCtx, log, job, res, err)

	cancel(nil)
	wg.Wait()
}

// result is what one attempt produced: how to publish it, and how to remove
// it if it is never published. Handlers fill in discard as soon as they have
// stored anything, even if they then fail.
type result struct {
	output  string                          // what was produced, for logs
	commit  func(ctx context.Context) error // the fenced write; nil if the handler already made it (Plan)
	discard func(ctx context.Context) error // removes stored output; nil if nothing was stored
}

// handle runs one attempt of whatever kind of job this is.
func (w *Worker) handle(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	switch {
	case job.Kind == jobs.KindChunk:
		return w.processChunk(ctx, log, job)
	case job.Kind == jobs.KindAudio:
		return w.processAudio(ctx, log, job)
	case job.Chunks != nil: // a root whose children have all committed
		return w.finalize(ctx, log, job)
	default:
		return w.processUpload(ctx, log, job)
	}
}

// processSingle is the single-file path: one ffmpeg run, one MP4.
func (w *Worker) processSingle(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	key, err := w.process(ctx, log, job)
	if key == "" {
		return result{}, err
	}
	return result{
		output:  key,
		commit:  func(ctx context.Context) error { return w.Jobs.Commit(ctx, job.ID, job.Fence, key) },
		discard: func(ctx context.Context) error { return w.Storage.Delete(ctx, key) },
	}, err
}

// errPermanent marks a failure that no retry can fix.
var errPermanent = errors.New("permanent")

// finish records the outcome of one attempt: a fenced commit on success, a
// fenced failure (retry or give up) otherwise, and nothing at all if the job
// was lost or the worker is shutting down. Output that is known not to be
// committed is deleted; nothing points at it.
func (w *Worker) finish(ctx, jobCtx context.Context, log *slog.Logger, job jobs.Job, res result, err error) {
	// The outcome must be written even if ctx is cancelled during the call.
	opCtx, opCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer opCancel()

	keep := false
	defer func() {
		if res.discard == nil || keep {
			return
		}
		if derr := res.discard(opCtx); derr != nil {
			log.Warn("delete uncommitted output", "output", res.output, "err", derr)
		}
	}()

	// The handler already made its fenced write (a plan), so the attempt is
	// over whatever the heartbeat saw afterwards.
	if err == nil && res.commit == nil {
		keep = true
		log.Info("job step done", "output", res.output)
		return
	}

	if cause := context.Cause(jobCtx); cause != nil {
		if ctx.Err() != nil {
			log.Info("worker shutting down; job left for the reaper")
		} else {
			log.Warn("stopped work on job", "reason", cause)
		}
		return
	}

	if err == nil {
		w.chaosBeforeCommit(log)
		// A pause above may outlast opCtx; give the commit its own deadline.
		opCtx, opCancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer opCancel()
		cerr := res.commit(opCtx)
		switch {
		case cerr == nil:
			keep = true
			log.Info("job done", "output", res.output)
		case errors.Is(cerr, jobs.ErrLeaseLost):
			log.Warn("commit rejected: a newer worker owns this job; result discarded", "output", res.output)
		default:
			// The commit may have reached the database before the error, in which
			// case a done job points at this file. Keep it; if the commit did not
			// land, the job is reaped and retried.
			keep = true
			log.Error("commit outcome unknown", "err", cerr)
		}
		return
	}

	if errors.Is(err, jobs.ErrLeaseLost) {
		log.Warn("lost job before commit", "err", err)
		return
	}
	permanent := errors.Is(err, media.ErrBadSource) || errors.Is(err, errPermanent)
	state, ferr := w.Jobs.Fail(opCtx, job.ID, job.Fence, err.Error(), permanent)
	switch {
	case ferr == nil:
		log.Warn("attempt failed", "err", err, "permanent", permanent, "new_state", state)
	case errors.Is(ferr, jobs.ErrLeaseLost):
		log.Warn("failure not recorded: a newer worker owns this job", "err", err)
	default:
		log.Error("record failure", "err", ferr, "attempt_err", err)
	}
}

// heartbeat renews the lease every LeaseTTL/3 until ctx is done. It cancels
// the job if the lease is lost, or if renewals fail until the lease deadline
// passes.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, job jobs.Job, deadline time.Time) {
	interval := w.LeaseTTL / 3
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(deadline)):
			cancel(errLeaseExpired)
			return
		case <-tick.C:
		}

		start := time.Now()
		callCtx, callCancel := context.WithTimeout(ctx, interval)
		err := w.Jobs.Heartbeat(callCtx, job.ID, job.Fence, w.LeaseTTL)
		callCancel()
		switch {
		case err == nil:
			deadline = start.Add(w.LeaseTTL)
		case errors.Is(err, jobs.ErrLeaseLost):
			cancel(jobs.ErrLeaseLost)
			return
		case ctx.Err() != nil:
			return
		default:
			w.Log.Error("heartbeat failed", "job", job.ID, "fence", job.Fence, "err", err,
				"lease_left", time.Until(deadline).Round(time.Millisecond))
		}
	}
}

// reapLoop returns expired leases to the queue every ReapInterval.
func (w *Worker) reapLoop(ctx context.Context) {
	tick := time.NewTicker(w.ReapInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		reaped, err := w.Jobs.ReapExpired(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.Error("reap failed", "err", err)
			}
			continue
		}
		for _, j := range reaped {
			w.Log.Warn("reaped expired lease", "job", j.ID, "fence", j.Fence,
				"attempt", j.Attempts, "new_state", j.State, "reason", *j.LastError)
		}
	}
}

// process makes and checks one proxy: probe the source, transcode, store the
// proxy under a key that includes the fence (so a stale worker and the
// current owner never write the same file), move the job to validating, and
// validate the stored copy. It returns the proxy's key whenever one was
// stored, so the caller can commit it or clean it up.
func (w *Worker) process(ctx context.Context, log *slog.Logger, job jobs.Job) (key string, err error) {
	dir, err := os.MkdirTemp(w.WorkDir, fmt.Sprintf("job-%s-f%d-", job.ID, job.Fence))
	if err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	src, err := storage.LocalFile(ctx, w.Storage, job.SourceKey, dir)
	if err != nil {
		return "", fmt.Errorf("fetch source %s: %w", job.SourceKey, err)
	}
	duration, err := media.CheckSource(ctx, src)
	if err != nil {
		return "", err
	}

	out := filepath.Join(dir, "proxy.mp4")
	start := time.Now()
	log.Info("transcoding", "source", job.SourceKey, "duration", duration.Round(time.Millisecond))
	onProgress, stopProgress := w.reportProgress(ctx, log, job, duration)
	err = media.Transcode(ctx, src, out, w.Proxy, onProgress)
	stopProgress()
	if err != nil {
		return "", err
	}

	key = fmt.Sprintf("proxies/%s/%d.mp4", job.ID, job.Fence)
	n, err := putFile(ctx, w.Storage, key, out)
	if err != nil {
		return "", fmt.Errorf("store proxy: %w", err) // a failed Put leaves nothing under key
	}
	log.Info("proxy written", "key", key, "bytes", n, "took", time.Since(start).Round(time.Millisecond))
	if err := w.chaosAfterStore(ctx, log, key, dir); err != nil {
		return key, err
	}

	if err := w.Jobs.MarkValidating(ctx, job.ID, job.Fence); err != nil {
		return key, err
	}
	// Validate what is actually in storage, not the temp file: this also
	// catches an upload that was cut short.
	stored, err := storage.LocalFile(ctx, w.Storage, key, dir)
	if err != nil {
		return key, fmt.Errorf("fetch stored proxy: %w", err)
	}
	if err := media.Validate(ctx, src, stored, w.Proxy); err != nil {
		return key, err
	}
	log.Info("proxy valid", "key", key)
	return key, nil
}

func putFile(ctx context.Context, s storage.Storage, key, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return s.Put(ctx, key, f)
}
