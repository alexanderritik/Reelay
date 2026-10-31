package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/hls"
	"github.com/alexanderritik/reelay/internal/jobs"
)

var testMedia = hls.Media{Init: "init.mp4", Segments: []hls.Segment{{URI: "seg_000.m4s", Duration: 2}}}

func specs(n int) []jobs.ChunkSpec {
	out := make([]jobs.ChunkSpec, n)
	for i := range out {
		out[i] = jobs.ChunkSpec{Start: float64(10 * i), End: float64(10 * (i + 1)), Frames: 300}
	}
	return out
}

// plannedRoot creates a root, claims it and plans n chunks (plus audio).
func plannedRoot(t *testing.T, store *jobs.Store, n int, withAudio bool) jobs.Job {
	t.Helper()
	root := claimOne(t, store)
	if err := store.Plan(context.Background(), root.ID, root.Fence, specs(n), withAudio); err != nil {
		t.Fatal(err)
	}
	return root
}

// claimChild claims the next job and checks it belongs to root.
func claimChild(t *testing.T, store *jobs.Store, owner, rootID string) jobs.Job {
	t.Helper()
	j, err := store.Claim(context.Background(), owner, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if j.ParentID == nil || *j.ParentID != rootID {
		t.Fatalf("claimed %s (kind %s), want a child of %s", j.ID, j.Kind, rootID)
	}
	return j
}

func TestPlanFansOutAndParksRoot(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	root := plannedRoot(t, store, 3, true)

	got, _ := store.Get(ctx, root.ID)
	if got.State != jobs.StateWaiting || got.PendingDeps != 4 || got.Chunks == nil || *got.Chunks != 3 {
		t.Errorf("root = state %s pending %d chunks %v, want waiting/4/3", got.State, got.PendingDeps, got.Chunks)
	}
	if got.LeaseOwner != nil {
		t.Error("waiting root still holds a lease")
	}

	kids, err := store.Children(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 4 {
		t.Fatalf("%d children, want 4", len(kids))
	}
	for i, k := range kids[:3] {
		if k.Kind != jobs.KindChunk || *k.ChunkIndex != i || k.SourceKey != root.SourceKey {
			t.Errorf("child %d = kind %s index %v source %s", i, k.Kind, k.ChunkIndex, k.SourceKey)
		}
	}
	if kids[3].Kind != jobs.KindAudio {
		t.Errorf("last child kind %s, want audio", kids[3].Kind)
	}

	// Claim order: audio first (playback needs it), then chunks in order.
	// The waiting root itself is never claimable.
	want := []string{"audio", "chunk 0", "chunk 1", "chunk 2"}
	for _, w := range want {
		j := claimChild(t, store, "w", root.ID)
		name := string(j.Kind)
		if j.ChunkIndex != nil {
			name = fmt.Sprintf("chunk %d", *j.ChunkIndex)
		}
		if name != w {
			t.Errorf("claimed %s, want %s", name, w)
		}
	}
	if _, err := store.Claim(ctx, "w", 30*time.Second); !errors.Is(err, jobs.ErrNoJob) {
		t.Errorf("claim with only a waiting root left: err = %v, want ErrNoJob", err)
	}
}

func TestPlanByStaleOwnerInsertsNothing(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	stale := claimOne(t, store)
	expireLease(t, pool, stale.ID)
	if _, err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "worker-b", 30*time.Second); err != nil {
		t.Fatal(err)
	}

	err := store.Plan(ctx, stale.ID, stale.Fence, specs(3), true)
	if !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale Plan: err = %v, want ErrLeaseLost", err)
	}
	kids, _ := store.Children(ctx, stale.ID)
	if len(kids) != 0 {
		t.Errorf("stale Plan inserted %d children, want 0", len(kids))
	}
}

// commitChild claims nothing: it moves an already claimed child through
// validating to done.
func commitChild(ctx context.Context, store *jobs.Store, j jobs.Job) error {
	if err := store.MarkValidating(ctx, j.ID, j.Fence); err != nil {
		return err
	}
	return store.CommitChild(ctx, j.ID, j.Fence, "proxies/x", testMedia)
}

func TestFanInConcurrentCommitsRequeueRootOnce(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	const n = 12
	root := plannedRoot(t, store, n, true)
	var kids []jobs.Job
	for i := range n + 1 {
		kids = append(kids, claimChild(t, store, fmt.Sprintf("w%d", i), root.ID))
	}

	// Every child commits at once. Each decrement must land exactly once and
	// exactly one of them must re-queue the root.
	var wg sync.WaitGroup
	errs := make(chan error, len(kids))
	start := make(chan struct{})
	for _, k := range kids {
		wg.Go(func() {
			<-start
			errs <- commitChild(ctx, store, k)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, _ := store.Get(ctx, root.ID)
	if got.State != jobs.StateQueued || got.PendingDeps != 0 || got.Attempts != 0 {
		t.Errorf("root = state %s pending %d attempts %d, want queued/0/0", got.State, got.PendingDeps, got.Attempts)
	}

	// The root comes back for its finalize step with a new fence.
	again, err := store.Claim(ctx, "finalizer", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != root.ID || again.Fence != root.Fence+1 || again.Chunks == nil {
		t.Errorf("finalize claim = %s fence %d, want root %s fence %d (planned)",
			again.ID, again.Fence, root.ID, root.Fence+1)
	}
}

func TestFanInStaleChildCommitDoesNotCountTwice(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	root := plannedRoot(t, store, 2, false)
	stale := claimChild(t, store, "worker-a", root.ID)

	// worker-a stalls; its lease runs out and worker-b redoes the chunk.
	expireLease(t, pool, stale.ID)
	if _, err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = now()`); err != nil {
		t.Fatal(err)
	}
	fresh := claimChild(t, store, "worker-b", root.ID)
	if fresh.ID != stale.ID || fresh.Fence != stale.Fence+1 {
		t.Fatalf("reclaim = %s fence %d, want %s fence %d", fresh.ID, fresh.Fence, stale.ID, stale.Fence+1)
	}
	if err := commitChild(ctx, store, fresh); err != nil {
		t.Fatal(err)
	}

	// worker-a wakes up and tries to commit the same chunk.
	if err := store.CommitChild(ctx, stale.ID, stale.Fence, "proxies/stale", testMedia); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("stale CommitChild: err = %v, want ErrLeaseLost", err)
	}
	got, _ := store.Get(ctx, root.ID)
	if got.PendingDeps != 1 || got.State != jobs.StateWaiting {
		t.Errorf("root = pending %d state %s, want 1/waiting (counted once)", got.PendingDeps, got.State)
	}
	child, _ := store.Get(ctx, fresh.ID)
	if child.OutputKey == nil || *child.OutputKey != "proxies/x" {
		t.Errorf("chunk output = %v, want the fresh worker's", child.OutputKey)
	}
}

func TestCommitChildRejectsRoot(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	job := validatingJob(t, store)
	if err := store.CommitChild(context.Background(), job.ID, job.Fence, "k", testMedia); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("CommitChild on a root: err = %v, want ErrLeaseLost", err)
	}
}

// assertGraphFailed checks that the root failed citing cause, and that every
// child other than the culprit and committed ones was cancelled.
func assertGraphFailed(t *testing.T, store *jobs.Store, rootID, cause string) {
	t.Helper()
	ctx := context.Background()
	root, _ := store.Get(ctx, rootID)
	if root.State != jobs.StateFailed || root.LastError == nil || !strings.Contains(*root.LastError, cause) {
		t.Errorf("root = %s (%v), want failed citing %q", root.State, root.LastError, cause)
	}
	kids, _ := store.Children(ctx, rootID)
	for _, k := range kids {
		if k.State != jobs.StateFailed && k.State != jobs.StateDone {
			t.Errorf("child %s still %s after the graph failed", k.ID, k.State)
		}
		if k.LeaseOwner != nil {
			t.Errorf("failed child %s still holds a lease", k.ID)
		}
	}
}

func TestChildPermanentFailureFailsGraph(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	root := plannedRoot(t, store, 3, true)
	audio := claimChild(t, store, "w1", root.ID)
	if err := commitChild(ctx, store, audio); err != nil {
		t.Fatal(err)
	}
	c0 := claimChild(t, store, "w2", root.ID)
	c1 := claimChild(t, store, "w3", root.ID) // still running when c0 fails
	// chunk 2 is still queued.

	state, err := store.Fail(ctx, c0.ID, c0.Fence, "bad frames", true)
	if err != nil || state != jobs.StateFailed {
		t.Fatalf("Fail = %s, %v; want failed", state, err)
	}
	assertGraphFailed(t, store, root.ID, "chunk 0 failed")

	// The running sibling learns on its next heartbeat and stops.
	if err := store.Heartbeat(ctx, c1.ID, c1.Fence, 30*time.Second); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("sibling heartbeat: err = %v, want ErrLeaseLost", err)
	}
	if _, err := store.Claim(ctx, "w4", 30*time.Second); !errors.Is(err, jobs.ErrNoJob) {
		t.Errorf("claim after graph failed: err = %v, want ErrNoJob", err)
	}
	if a, _ := store.Get(ctx, audio.ID); a.State != jobs.StateDone {
		t.Errorf("committed audio = %s, want it left done", a.State)
	}
}

func TestChildRetryableFailureLeavesGraphAlone(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	root := plannedRoot(t, store, 2, false)
	c0 := claimChild(t, store, "w", root.ID)
	state, err := store.Fail(ctx, c0.ID, c0.Fence, "ffmpeg crashed", false)
	if err != nil || state != jobs.StateQueued {
		t.Fatalf("Fail = %s, %v; want queued", state, err)
	}
	got, _ := store.Get(ctx, root.ID)
	if got.State != jobs.StateWaiting {
		t.Errorf("root = %s, want still waiting", got.State)
	}
}

func TestReapedChildOutOfAttemptsFailsGraph(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	root := plannedRoot(t, store, 2, false)
	setMaxAttempts(t, pool, 1)
	c0 := claimChild(t, store, "worker-a", root.ID)
	expireLease(t, pool, c0.ID)
	if _, err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	assertGraphFailed(t, store, root.ID, "worker-a stopped heartbeating")
}

func setMaxAttempts(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE jobs SET max_attempts = $1 WHERE kind <> 'proxy'`, n); err != nil {
		t.Fatal(err)
	}
}
