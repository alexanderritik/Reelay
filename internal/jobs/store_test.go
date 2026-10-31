package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/db"
	"github.com/alexanderritik/reelay/internal/jobs"
)

// testPool connects to a dedicated reelay_test database on the dev server
// (docker-compose up), creating it if needed, and empties the jobs table so
// each test starts clean. Skips the test if Postgres is not running.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	adminURL := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminURL == "" {
		adminURL = "postgres://reelay:reelay@localhost:5435/reelay"
	}
	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	var exists bool
	if err := admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'reelay_test')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := admin.Exec(ctx, `CREATE DATABASE reelay_test`); err != nil {
			t.Fatal(err)
		}
	}
	admin.Close()

	cfg := admin.Config().ConnConfig
	pool, err := db.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%d/reelay_test",
		cfg.User, cfg.Password, cfg.Host, cfg.Port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE jobs`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestClaimConcurrentWorkersEachJobOnce(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	const nJobs, nWorkers = 10, 8
	for range nJobs {
		if _, _, err := store.Create(ctx, uuid.NewString(), "sources/x.mp4", nil); err != nil {
			t.Fatal(err)
		}
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims = map[string][]jobs.Job{} // job ID -> every claim of it
	)
	for w := range nWorkers {
		owner := fmt.Sprintf("worker-%d", w)
		wg.Go(func() {
			for {
				job, err := store.Claim(ctx, owner, 30*time.Second)
				if errors.Is(err, jobs.ErrNoJob) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				if job.LeaseOwner == nil || *job.LeaseOwner != owner {
					t.Errorf("job %s lease owner = %v, want %s", job.ID, job.LeaseOwner, owner)
				}
				mu.Lock()
				claims[job.ID] = append(claims[job.ID], job)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(claims) != nJobs {
		t.Fatalf("claimed %d distinct jobs, want %d", len(claims), nJobs)
	}
	for id, cs := range claims {
		if len(cs) != 1 {
			t.Errorf("job %s claimed %d times, want 1", id, len(cs))
			continue
		}
		j := cs[0]
		if j.State != jobs.StateRunning || j.Fence != 1 || j.Attempts != 1 {
			t.Errorf("job %s = state %s fence %d attempts %d, want running/1/1",
				id, j.State, j.Fence, j.Attempts)
		}
		if j.LeaseExpiresAt == nil || !j.LeaseExpiresAt.After(time.Now()) {
			t.Errorf("job %s lease expires at %v, want a time in the future", id, j.LeaseExpiresAt)
		}
	}
}

func TestClaimSkipsJobsWithNoAttemptsLeft(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (source_key, attempts, max_attempts) VALUES ('sources/x.mp4', 3, 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "w", 30*time.Second); !errors.Is(err, jobs.ErrNoJob) {
		t.Errorf("Claim err = %v, want ErrNoJob", err)
	}
}

func TestCreateConcurrentSameIdempotencyKey(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	key := "test-" + uuid.NewString()
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM jobs WHERE idempotency_key = $1`, key) })

	const n = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[string]bool{}
	)
	for range n {
		wg.Go(func() {
			job, ok, err := store.Create(ctx, uuid.NewString(), "sources/x.mp4", &key)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[job.ID] = true
			if ok {
				created++
			}
		})
	}
	wg.Wait()

	if created != 1 {
		t.Errorf("created = %d, want exactly 1", created)
	}
	if len(ids) != 1 {
		t.Errorf("callers saw %d distinct jobs, want 1", len(ids))
	}
}

func TestCreateWithoutKeyAlwaysInserts(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	a, okA, errA := store.Create(ctx, uuid.NewString(), "sources/a.mp4", nil)
	b, okB, errB := store.Create(ctx, uuid.NewString(), "sources/b.mp4", nil)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM jobs WHERE id = ANY($1)`, []string{a.ID, b.ID}) })
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if !okA || !okB || a.ID == b.ID {
		t.Errorf("want two distinct new jobs, got %v/%v %s %s", okA, okB, a.ID, b.ID)
	}
	if a.State != jobs.StateQueued || a.Fence != 0 || a.Attempts != 0 {
		t.Errorf("new job = %+v, want queued with fence 0 and attempts 0", a)
	}
}

// claimOne creates a job and claims it, returning the claimed job.
func claimOne(t *testing.T, store *jobs.Store) jobs.Job {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Create(ctx, uuid.NewString(), "sources/x.mp4", nil); err != nil {
		t.Fatal(err)
	}
	job, err := store.Claim(ctx, "worker-a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// expireLease moves a job's lease into the past, as if its worker had stopped
// heartbeating long ago.
func expireLease(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestReapRequeuesExpiredJobWithAttemptsLeft(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	job := claimOne(t, store)
	expireLease(t, pool, job.ID)

	reaped, err := store.ReapExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].ID != job.ID {
		t.Fatalf("reaped %v, want just %s", reaped, job.ID)
	}
	got, _ := store.Get(ctx, job.ID)
	if got.State != jobs.StateQueued || got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Errorf("after reap = %+v, want queued with lease cleared", got)
	}
	if got.Fence != job.Fence {
		t.Errorf("fence = %d, want unchanged %d (only Claim bumps it)", got.Fence, job.Fence)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "worker-a stopped heartbeating") {
		t.Errorf("last_error = %v, want it to name the dead worker", got.LastError)
	}

	// Crashes back off like errors: attempt 1 waits up to 2s.
	if got.AvailableAt.After(time.Now().Add(3 * time.Second)) {
		t.Errorf("available_at = %v, want within 2s", got.AvailableAt)
	}

	// Once the backoff has passed, the job is claimable again with a new fence.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	again, err := store.Claim(ctx, "worker-b", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != job.ID || again.Fence != job.Fence+1 || again.Attempts != 2 {
		t.Errorf("reclaim = id %s fence %d attempts %d, want %s/%d/2",
			again.ID, again.Fence, again.Attempts, job.ID, job.Fence+1)
	}
}

func TestReapFailsExpiredJobWithNoAttemptsLeft(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	job := claimOne(t, store)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET attempts = max_attempts WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	expireLease(t, pool, job.ID)

	if _, err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, job.ID)
	if got.State != jobs.StateFailed {
		t.Errorf("state = %s, want failed", got.State)
	}
}

func TestReapLeavesLiveLeasesAlone(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	job := claimOne(t, store)
	reaped, err := store.ReapExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 0 {
		t.Errorf("reaped %d jobs, want 0", len(reaped))
	}
	got, _ := store.Get(ctx, job.ID)
	if got.State != jobs.StateRunning {
		t.Errorf("state = %s, want running", got.State)
	}
}

func TestHeartbeat(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	job := claimOne(t, store)

	t.Run("owner extends lease", func(t *testing.T) {
		if err := store.Heartbeat(ctx, job.ID, job.Fence, time.Hour); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.ID)
		if !got.LeaseExpiresAt.After(time.Now().Add(50 * time.Minute)) {
			t.Errorf("lease expires at %v, want about an hour from now", got.LeaseExpiresAt)
		}
	})

	t.Run("stale fence is rejected", func(t *testing.T) {
		if err := store.Heartbeat(ctx, job.ID, job.Fence-1, time.Hour); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("same fence after reap is rejected", func(t *testing.T) {
		expireLease(t, pool, job.ID)
		if _, err := store.ReapExpired(ctx); err != nil {
			t.Fatal(err)
		}
		// Fence is unchanged until the next claim; the state check must catch it.
		if err := store.Heartbeat(ctx, job.ID, job.Fence, time.Hour); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
	})
}

func TestMarkValidating(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	job := claimOne(t, store)

	if err := store.MarkValidating(ctx, job.ID, job.Fence-1); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("stale fence: err = %v, want ErrLeaseLost", err)
	}
	if err := store.MarkValidating(ctx, job.ID, job.Fence); err != nil {
		t.Fatalf("owner: %v", err)
	}
	got, _ := store.Get(ctx, job.ID)
	if got.State != jobs.StateValidating {
		t.Errorf("state = %s, want validating", got.State)
	}
	if err := store.MarkValidating(ctx, job.ID, job.Fence); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("already validating: err = %v, want ErrLeaseLost", err)
	}
	// The lease still covers the validating state.
	if err := store.Heartbeat(ctx, job.ID, job.Fence, time.Minute); err != nil {
		t.Errorf("heartbeat while validating: %v", err)
	}
}

// validatingJob returns a claimed job that has reached validating.
func validatingJob(t *testing.T, store *jobs.Store) jobs.Job {
	t.Helper()
	job := claimOne(t, store)
	if err := store.MarkValidating(context.Background(), job.ID, job.Fence); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCommit(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	t.Run("owner commits", func(t *testing.T) {
		job := validatingJob(t, store)
		if err := store.Commit(ctx, job.ID, job.Fence, "proxies/x/1.mp4"); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.ID)
		if got.State != jobs.StateDone || got.OutputKey == nil || *got.OutputKey != "proxies/x/1.mp4" {
			t.Errorf("after commit = state %s output %v, want done with the proxy", got.State, got.OutputKey)
		}
		if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
			t.Errorf("lease not cleared: %v %v", got.LeaseOwner, got.LeaseExpiresAt)
		}
	})

	t.Run("stale fence is rejected and changes nothing", func(t *testing.T) {
		job := validatingJob(t, store)
		if err := store.Commit(ctx, job.ID, job.Fence-1, "proxies/x/old.mp4"); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
		got, _ := store.Get(ctx, job.ID)
		if got.State != jobs.StateValidating || got.OutputKey != nil {
			t.Errorf("after stale commit = state %s output %v, want untouched", got.State, got.OutputKey)
		}
	})

	t.Run("cannot skip validation", func(t *testing.T) {
		job := claimOne(t, store) // running, never validated
		if err := store.Commit(ctx, job.ID, job.Fence, "proxies/x/1.mp4"); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
	})
}

func TestFail(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	t.Run("attempts left: queued with backoff", func(t *testing.T) {
		job := validatingJob(t, store)
		before := time.Now()
		state, err := store.Fail(ctx, job.ID, job.Fence, "invalid proxy: too short", false)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.ID)
		if state != jobs.StateQueued || got.State != jobs.StateQueued {
			t.Errorf("state = %s / %s, want queued", state, got.State)
		}
		// Attempt 1: full jitter in [0, 2s).
		if got.AvailableAt.Before(before.Add(-time.Second)) || got.AvailableAt.After(before.Add(3*time.Second)) {
			t.Errorf("available_at = %v, want within 2s of %v", got.AvailableAt, before)
		}
		if got.LastError == nil || *got.LastError != "attempt 1 of 3: invalid proxy: too short" {
			t.Errorf("last_error = %v", got.LastError)
		}
		if got.LeaseOwner != nil {
			t.Errorf("lease not cleared")
		}
	})

	t.Run("last attempt: failed", func(t *testing.T) {
		job := claimOne(t, store)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET attempts = max_attempts WHERE id = $1`, job.ID); err != nil {
			t.Fatal(err)
		}
		if state, err := store.Fail(ctx, job.ID, job.Fence, "ffmpeg crashed", false); err != nil || state != jobs.StateFailed {
			t.Errorf("state = %s, err = %v, want failed", state, err)
		}
	})

	t.Run("permanent: failed with attempts left", func(t *testing.T) {
		job := claimOne(t, store)
		if state, err := store.Fail(ctx, job.ID, job.Fence, "bad source", true); err != nil || state != jobs.StateFailed {
			t.Errorf("state = %s, err = %v, want failed", state, err)
		}
	})

	t.Run("stale fence is rejected", func(t *testing.T) {
		job := claimOne(t, store)
		if _, err := store.Fail(ctx, job.ID, job.Fence-1, "x", false); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
	})
}

func TestClaimWaitsForBackoff(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO jobs (source_key, available_at)
		VALUES ('sources/x.mp4', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "w", 30*time.Second); !errors.Is(err, jobs.ErrNoJob) {
		t.Errorf("Claim err = %v, want ErrNoJob while backing off", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "w", 30*time.Second); err != nil {
		t.Errorf("Claim after backoff: %v", err)
	}
}

func TestProgress(t *testing.T) {
	pool := testPool(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()
	progressOf := func(id string) float64 {
		t.Helper()
		j, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Progress
	}

	job := claimOne(t, store)
	if p := progressOf(job.ID); p != 0 {
		t.Errorf("new claim progress = %v, want 0", p)
	}

	t.Run("owner writes progress", func(t *testing.T) {
		if err := store.SetProgress(ctx, job.ID, job.Fence, 42.5); err != nil {
			t.Fatal(err)
		}
		if p := progressOf(job.ID); p != 42.5 {
			t.Errorf("progress = %v, want 42.5", p)
		}
	})

	t.Run("stale fence cannot overwrite it", func(t *testing.T) {
		if err := store.SetProgress(ctx, job.ID, job.Fence-1, 5); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
		if p := progressOf(job.ID); p != 42.5 {
			t.Errorf("progress = %v, want 42.5 unchanged", p)
		}
	})

	t.Run("failed attempt resets it", func(t *testing.T) {
		if _, err := store.Fail(ctx, job.ID, job.Fence, "boom", false); err != nil {
			t.Fatal(err)
		}
		if p := progressOf(job.ID); p != 0 {
			t.Errorf("progress after Fail = %v, want 0", p)
		}
	})

	t.Run("commit sets 100", func(t *testing.T) {
		j := validatingJob(t, store)
		if err := store.SetProgress(ctx, j.ID, j.Fence, 10); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Errorf("SetProgress while validating: err = %v, want ErrLeaseLost (only running jobs report)", err)
		}
		if err := store.Commit(ctx, j.ID, j.Fence, "proxies/x/1.mp4"); err != nil {
			t.Fatal(err)
		}
		if p := progressOf(j.ID); p != 100 {
			t.Errorf("progress after Commit = %v, want 100", p)
		}
	})
}
