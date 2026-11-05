package main

// Failure scenarios for chunked proxies: a source split into chunk and audio
// jobs (worker/chunked.go) whose commits fan in to the upload's job
// (jobs/graph.go). Only these scenarios' workers get the chunking settings,
// so every other scenario still takes the single-file path.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
)

// The "chunked" source is 40s, so it splits into 10 chunks plus audio.
var chunkWorkerEnv = []string{"CHUNK_SECONDS=4s", "CHUNK_MIN_DURATION=10s"}

func (h *harness) startChunkWorker(name string, env ...string) *proc {
	return h.startWorker(name, append(slices.Clone(chunkWorkerEnv), env...)...)
}

// ownedBy finds the child that p currently holds a lease on.
func (h *harness) ownedBy(ctx context.Context, rootID string, p *proc) (jobs.Job, error) {
	kids, err := h.jobs.Children(ctx, rootID)
	if err != nil {
		return jobs.Job{}, err
	}
	for _, k := range kids {
		if k.LeaseOwner != nil && strings.Contains(*k.LeaseOwner, fmt.Sprintf("-%d-", p.pid())) {
			return k, nil
		}
	}
	return jobs.Job{}, fmt.Errorf("%s holds no child of %s", p.name, rootID[:8])
}

func childName(k jobs.Job) string {
	if k.ChunkIndex != nil {
		return fmt.Sprintf("chunk %d", *k.ChunkIndex)
	}
	return string(k.Kind)
}

func killMidChunk(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	a, b := h.startChunkWorker("8-A"), h.startChunkWorker("8-B")
	if err := a.waitLog("transcoding chunk", 30*time.Second); err != nil {
		return "", err
	}
	time.Sleep(300 * time.Millisecond)
	victim, err := h.ownedBy(ctx, id, a)
	if err != nil {
		return "", err
	}
	a.kill()
	time.Sleep(500 * time.Millisecond)
	orphans := ffmpegPIDs(victim.ID)
	h.startChunkWorker("8-C")

	root, err := h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
	if err != nil {
		return "", err
	}
	b.stop()
	if len(orphans) > 0 {
		return "", fmt.Errorf("killed worker left ffmpeg running (pid %v)", orphans)
	}
	kids, err := h.jobs.Children(ctx, id)
	if err != nil {
		return "", err
	}
	redone := 0
	for _, k := range kids {
		switch {
		case k.ID == victim.ID && k.Fence != victim.Fence+1:
			return "", fmt.Errorf("killed %s done at fence %d, want %d", childName(k), k.Fence, victim.Fence+1)
		case k.ID != victim.ID && k.Fence != 1:
			return "", fmt.Errorf("%s was redone (fence %d) though its worker was healthy", childName(k), k.Fence)
		case k.Fence > 1:
			redone++
		}
	}
	if err := h.validateOutput(ctx, root); err != nil {
		return "", fmt.Errorf("assembled proxy: %w", err)
	}
	return fmt.Sprintf("A killed mid-%s; only that chunk was redone (fence %d), the other %d children kept "+
		"their first result; seams re-validated, root done", childName(victim), victim.Fence+1, len(kids)-redone), nil
}

func freezeChildPastLease(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	a := h.startChunkWorker("9-A", "CONDUIT_CHAOS=freeze-before-commit")
	if err := a.waitLog("chaos: freezing", 60*time.Second); err != nil {
		return "", err
	}
	if err := waitFor(5*time.Second, func() bool { return procState(a.pid()) == 'T' }); err != nil {
		return "", errors.New("worker A did not freeze")
	}
	victim, err := h.ownedBy(ctx, id, a)
	if err != nil {
		return "", err
	}

	b := h.startChunkWorker("9-B")
	root, err := h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
	if err != nil {
		return "", err
	}
	b.stop()

	a.signal(syscall.SIGCONT)
	if err := a.waitLog("commit rejected", 10*time.Second); err != nil {
		return "", fmt.Errorf("A resumed but its stale child commit was not rejected: %w", err)
	}
	root, err = h.jobs.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if root.State != jobs.StateDone || root.PendingDeps != 0 || root.Fence != 2 {
		return "", fmt.Errorf("root after A's late commit: %s, pending %d, fence %d; want done, 0, 2 "+
			"(planned once, finalized once)", root.State, root.PendingDeps, root.Fence)
	}
	stale := h.dataPath(staleKey(id, victim))
	if err := waitFor(3*time.Second, func() bool { return !exists(stale) }); err != nil {
		return "", fmt.Errorf("A's rejected %s output was not deleted", childName(victim))
	}
	return fmt.Sprintf("A froze before committing %s (fence %d); B redid it and the rest; on resume A's "+
		"commit matched 0 rows, so the fan-in counted it once: root finalized once (fence 2), stale output deleted",
		childName(victim), victim.Fence), nil
}

func staleKey(rootID string, k jobs.Job) string {
	if k.Kind == jobs.KindAudio {
		return fmt.Sprintf("proxies/%s/a/%d", rootID, k.Fence)
	}
	return fmt.Sprintf("proxies/%s/v/%d/%d", rootID, *k.ChunkIndex, k.Fence)
}

func childFailsEveryAttempt(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	a := h.startChunkWorker("10-A", "CONDUIT_CHAOS=truncate-proxy")
	root, err := h.waitState(ctx, id, jobs.StateFailed, 120*time.Second)
	if err != nil {
		return "", err
	}
	a.stop()
	kids, err := h.jobs.Children(ctx, id)
	if err != nil {
		return "", err
	}
	var culprit *jobs.Job
	cancelled := 0
	for i, k := range kids {
		switch {
		case k.State != jobs.StateFailed:
			return "", fmt.Errorf("%s left %s after the graph failed", childName(k), k.State)
		case strings.HasPrefix(deref(k.LastError), "cancelled: "):
			cancelled++
		case k.Attempts == k.MaxAttempts:
			culprit = &kids[i]
		}
	}
	if culprit == nil {
		return "", errors.New("no child used all its attempts")
	}
	if !strings.Contains(deref(root.LastError), childName(*culprit)+" failed") {
		return "", fmt.Errorf("root last_error = %q, want it to name %s", deref(root.LastError), childName(*culprit))
	}
	return fmt.Sprintf("%s failed %d/%d attempts; in the same transaction the root failed (%q) and "+
		"%d unfinished siblings were cancelled", childName(*culprit), culprit.Attempts, culprit.MaxAttempts,
		shorten(deref(root.LastError), 60), cancelled), nil
}

func databaseRestartFanOut(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	w1, w2 := h.startChunkWorker("11-W1"), h.startChunkWorker("11-W2")
	committed := 0
	if err := waitFor(60*time.Second, func() bool {
		kids, err := h.jobs.Children(ctx, id)
		committed = 0
		for _, k := range kids {
			if k.State == jobs.StateDone {
				committed++
			}
		}
		return err == nil && committed > 0 && committed < len(kids)
	}); err != nil {
		return "", errors.New("fan-out never got going")
	}

	start := time.Now()
	if out, err := exec.Command("docker", "restart", h.pgContainer).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker restart: %v: %s", err, out)
	}
	down := time.Since(start)

	root, err := h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
	if err != nil {
		return "", err
	}
	w1.stop()
	w2.stop()
	if err := h.validateOutput(ctx, root); err != nil {
		return "", fmt.Errorf("assembled proxy: %w", err)
	}
	kids, _ := h.jobs.Children(ctx, id)
	retried := 0
	for _, k := range kids {
		if k.Fence > 1 {
			retried++
		}
	}
	return fmt.Sprintf("Postgres restarted with %d/%d children committed (%s outage); the rest finished "+
		"(%d needed a retry), root finalized and seams re-validated", committed, len(kids),
		down.Round(100*time.Millisecond), retried), nil
}

func crashDuringPlan(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	a := h.startChunkWorker("12-A", "CONDUIT_CHAOS=crash-during-plan")
	if err := waitFor(30*time.Second, a.exited); err != nil {
		return "", errors.New("worker A did not crash while planning")
	}
	kids, err := h.jobs.Children(ctx, id)
	if err != nil {
		return "", err
	}
	if len(kids) != 0 {
		return "", fmt.Errorf("crashed planner left %d children behind", len(kids))
	}

	b := h.startChunkWorker("12-B")
	root, err := h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
	if err != nil {
		return "", err
	}
	b.stop()
	kids, err = h.jobs.Children(ctx, id)
	if err != nil {
		return "", err
	}
	if root.Chunks == nil || len(kids) != *root.Chunks+1 {
		return "", fmt.Errorf("%d children for %v planned chunks + audio: planned more than once", len(kids), root.Chunks)
	}
	if root.Fence != 3 {
		return "", fmt.Errorf("root done at fence %d, want 3 (A's plan, B's plan, B's finalize)", root.Fence)
	}
	if err := h.validateOutput(ctx, root); err != nil {
		return "", fmt.Errorf("assembled proxy: %w", err)
	}
	return fmt.Sprintf("A was killed after splitting the source but before writing the plan: 0 children existed; "+
		"B reclaimed the root and planned once (%d chunks + audio), root done at fence 3", *root.Chunks), nil
}

func crashDuringFinalize(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "chunked")
	if err != nil {
		return "", err
	}
	a := h.startChunkWorker("13-A", "CONDUIT_CHAOS=crash-during-finalize")
	if err := waitFor(120*time.Second, a.exited); err != nil {
		return "", errors.New("worker A did not crash while finalizing")
	}
	root, err := h.jobs.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if root.State != jobs.StateValidating {
		return "", fmt.Errorf("root %s after A crashed, want validating", root.State)
	}
	crashedAt := root.Fence

	b := h.startChunkWorker("13-B")
	root, err = h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
	if err != nil {
		return "", err
	}
	b.stop()
	kids, err := h.jobs.Children(ctx, id)
	if err != nil {
		return "", err
	}
	for _, k := range kids {
		if k.Fence != 1 {
			return "", fmt.Errorf("%s was redone (fence %d); only the finalize step should rerun", childName(k), k.Fence)
		}
	}
	if root.Fence != crashedAt+1 {
		return "", fmt.Errorf("root done at fence %d, want %d (one more claim)", root.Fence, crashedAt+1)
	}
	if err := h.validateOutput(ctx, root); err != nil {
		return "", fmt.Errorf("assembled proxy: %w", err)
	}
	return fmt.Sprintf("A was killed mid-finalize (root validating, fence %d); its lease expired and B reran only "+
		"the finalize step: all %d children kept fence 1, root done at fence %d", crashedAt, len(kids), root.Fence), nil
}

// validateGraphOutput checks a done chunk, audio or chunked root the way the
// worker did before committing it, against what is in storage now.
func (h *harness) validateGraphOutput(ctx context.Context, j jobs.Job, src string) error {
	stored := func(k jobs.Job) (media.ChunkOutput, error) {
		if k.OutputKey == nil || k.HLS == nil {
			return media.ChunkOutput{}, fmt.Errorf("%s is done without output", childName(k))
		}
		dir := h.dataPath(*k.OutputKey)
		for _, f := range media.OutputFiles(*k.HLS) {
			if !exists(filepath.Join(dir, filepath.FromSlash(f))) {
				return media.ChunkOutput{}, fmt.Errorf("output %s/%s missing", *k.OutputKey, f)
			}
		}
		return media.ChunkOutput{Dir: dir, HLS: *k.HLS}, nil
	}
	switch j.Kind {
	case jobs.KindChunk:
		c, err := stored(j)
		if err != nil {
			return err
		}
		return media.ValidateChunk(ctx, src, c, media.DefaultProxy, *j.RangeStart, *j.RangeEnd, *j.ExpectedFrames)
	case jobs.KindAudio:
		c, err := stored(j)
		if err != nil {
			return err
		}
		return media.ValidateAudio(ctx, src, filepath.Join(c.Dir, media.PlaylistName))
	}

	kids, err := h.jobs.Children(ctx, j.ID)
	if err != nil {
		return err
	}
	var chunks []media.ChunkOutput
	for _, k := range kids {
		if k.Kind != jobs.KindChunk {
			continue
		}
		if k.State != jobs.StateDone {
			return fmt.Errorf("root done but %s is %s", childName(k), k.State)
		}
		c, err := stored(k)
		if err != nil {
			return err
		}
		chunks = append(chunks, c)
	}
	frames, err := media.SourceFrames(ctx, src)
	if err != nil {
		return err
	}
	return media.ValidateAssembly(ctx, src, chunks, media.DefaultProxy, len(frames))
}

// committedFile reports whether a stored proxy file belongs to a commit:
// either a single-file proxy itself, or a file under a committed child's
// prefix. Anything else is a leftover from an attempt that did not commit.
func committedFile(rel string, committed map[string]bool) bool {
	if committed[rel] {
		return true
	}
	for dir := filepath.Dir(rel); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
		if committed[filepath.ToSlash(dir)+"/"] {
			return true
		}
	}
	return false
}

// proxyFiles lists every stored proxy file, relative to the data dir.
func (h *harness) proxyFiles() []string {
	var files []string
	root := h.dataPath("proxies")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(h.dataPath(""), p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}
