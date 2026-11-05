// Package jobs is the durable job table: every state transition lives here.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/hls"
)

type State string

const (
	StateQueued     State = "queued"
	StateRunning    State = "running"
	StateValidating State = "validating"
	StateWaiting    State = "waiting" // a root whose chunk jobs are running; see graph.go
	StateDone       State = "done"
	StateFailed     State = "failed"
)

var (
	ErrNotFound  = errors.New("job not found")
	ErrNoJob     = errors.New("no queued job to claim")
	ErrLeaseLost = errors.New("lease lost: job was reclaimed or reaped")
)

type Job struct {
	ID             string     `json:"id"`
	State          State      `json:"state"`
	SourceKey      string     `json:"source_key"`
	OutputKey      *string    `json:"output_key,omitempty"`
	Fence          int64      `json:"fence"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	LeaseOwner     *string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	LastError      *string    `json:"last_error,omitempty"`
	IdempotencyKey *string    `json:"idempotency_key,omitempty"`
	AvailableAt    time.Time  `json:"available_at"`
	Progress       float64    `json:"progress"` // transcode % of the current attempt
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Chunked proxies (see graph.go). Zero values for the single-file path.
	Kind           Kind        `json:"kind"`
	ParentID       *string     `json:"parent_id,omitempty"`
	ChunkIndex     *int        `json:"chunk_index,omitempty"`
	RangeStart     *float64    `json:"range_start,omitempty"`
	RangeEnd       *float64    `json:"range_end,omitempty"`
	ExpectedFrames *int        `json:"expected_frames,omitempty"`
	Priority       int         `json:"priority"`
	Chunks         *int        `json:"chunks,omitempty"` // root: video chunks, once planned
	PendingDeps    int         `json:"pending_deps"`
	HLS            *hls.Output `json:"hls,omitempty"` // child: what its commit published
}

const columns = `id, state, source_key, output_key, fence, attempts, max_attempts,
	lease_owner, lease_expires_at, last_error, idempotency_key, available_at, progress, created_at, updated_at,
	kind, parent_id, chunk_index, range_start, range_end, expected_frames, priority, chunks, pending_deps, hls`

func scan(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.State, &j.SourceKey, &j.OutputKey, &j.Fence, &j.Attempts,
		&j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt, &j.LastError, &j.IdempotencyKey,
		&j.AvailableAt, &j.Progress, &j.CreatedAt, &j.UpdatedAt,
		&j.Kind, &j.ParentID, &j.ChunkIndex, &j.RangeStart, &j.RangeEnd, &j.ExpectedFrames,
		&j.Priority, &j.Chunks, &j.PendingDeps, &j.HLS)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a queued job. If idemKey is already taken, nothing is
// inserted and the existing job is returned with created=false. Concurrent
// calls with the same key are safe: the unique index picks one winner and
// the others wait for it to commit, then read its row.
func (s *Store) Create(ctx context.Context, id, sourceKey string, idemKey *string) (job Job, created bool, err error) {
	job, err = scan(s.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, source_key, idempotency_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+columns, id, sourceKey, idemKey))
	if err == nil {
		return job, true, nil
	}
	if !errors.Is(err, ErrNotFound) || idemKey == nil {
		return Job{}, false, err
	}
	job, err = s.GetByIdempotencyKey(ctx, *idemKey)
	return job, false, err
}

// Claim takes ownership of the next queued job that has attempts left, in a
// single statement: it moves the job to running, increments fence (the new
// owner's token) and attempts, and sets a lease measured on the database
// clock. Higher priority goes first, then older jobs, then lower chunk index,
// so audio and early chunks of started jobs come before new uploads (see
// graph.go). SKIP LOCKED lets concurrent claimers pass over a row another claimer
// holds instead of waiting on it, so each job goes to exactly one worker.
// Returns ErrNoJob when there is nothing to claim.
func (s *Store) Claim(ctx context.Context, owner string, lease time.Duration) (Job, error) {
	job, err := scan(s.pool.QueryRow(ctx, `
		UPDATE jobs
		SET state            = 'running',
		    fence            = fence + 1,
		    attempts         = attempts + 1,
		    progress         = 0,
		    lease_owner      = $1,
		    lease_expires_at = now() + make_interval(secs => $2),
		    updated_at       = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE state = 'queued' AND attempts < max_attempts AND available_at <= now()
			ORDER BY priority DESC, created_at, chunk_index
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+columns, owner, lease.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrNoJob
	}
	return job, err
}

// Heartbeat extends the lease of a job the caller still owns. It returns
// ErrLeaseLost if the fence has moved on (another worker claimed the job) or
// the job is no longer active (the reaper returned it to the queue — the
// fence is unchanged until the next claim, so the state check is required).
func (s *Store) Heartbeat(ctx context.Context, id string, fence int64, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET lease_expires_at = now() + make_interval(secs => $3),
		    updated_at       = now()
		WHERE id = $1 AND fence = $2 AND state IN ('running', 'validating')`,
		id, fence, lease.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// MarkValidating moves a job the caller owns from running to validating.
// It returns ErrLeaseLost if the fence has moved on or the job is no longer
// running.
func (s *Store) MarkValidating(ctx context.Context, id string, fence int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET state = 'validating', updated_at = now()
		WHERE id = $1 AND fence = $2 AND state = 'running'`,
		id, fence)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// SetProgress records transcode progress for the holder of the current
// fence. A stale worker gets ErrLeaseLost, so it can never overwrite the new
// owner's progress.
func (s *Store) SetProgress(ctx context.Context, id string, fence int64, percent float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET progress = $3, updated_at = now()
		WHERE id = $1 AND fence = $2 AND state = 'running'`,
		id, fence, min(max(percent, 0), 100))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Commit marks a validated job done with its proxy, but only for the holder
// of the current fence. A stale worker gets ErrLeaseLost and its result is
// discarded. The fence decides, not the clock: if the lease ran out but no
// one has reclaimed the job, the owner's commit is still the newest and is
// accepted.
func (s *Store) Commit(ctx context.Context, id string, fence int64, outputKey string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET state            = 'done',
		    output_key       = $3,
		    progress         = 100,
		    lease_owner      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = now()
		WHERE id = $1 AND fence = $2 AND state = 'validating'`,
		id, fence, outputKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// backoffSQL is the delay before a failed job may be claimed again: "full
// jitter", a random wait between 0 and min(60s, 2s * 2^(attempts-1)). The
// randomness spreads out retries of jobs that failed together. Shared by
// Fail and ReapExpired so crashes back off the same way as errors.
const backoffSQL = `make_interval(secs => random() * least(60, 2 * power(2, attempts - 1)))`

// Fail records a failed attempt by the holder of the current fence. The job
// goes back to queued (after a backoff) if it has attempts left and the
// failure is not permanent; otherwise it ends failed. Returns the new state,
// or ErrLeaseLost if the caller no longer owns the job. A chunk or audio job
// that ends failed takes its root and siblings with it (graph.go).
func (s *Store) Fail(ctx context.Context, id string, fence int64, reason string, permanent bool) (state State, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE jobs
			SET state            = CASE WHEN $4 OR attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			    last_error       = format('attempt %s of %s: %s', attempts, max_attempts, $3::text),
			    available_at     = now() + `+backoffSQL+`,
			    progress         = 0,
			    lease_owner      = NULL,
			    lease_expires_at = NULL,
			    updated_at       = now()
			WHERE id = $1 AND fence = $2 AND state IN ('running', 'validating')
			RETURNING state`,
			id, fence, reason, permanent).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil || state != StateFailed {
			return err
		}
		return failDependents(ctx, tx)
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

// ReapExpired releases every active job whose lease has run out: back to
// queued (after a backoff) if it has attempts left, otherwise failed. The fence is left alone;
// the next Claim increments it. Safe to run from many workers at once. If a
// heartbeat renews a row concurrently, Postgres re-checks the WHERE clause
// against the renewed row, so a just-renewed lease is never reaped. A child
// that runs out of attempts here fails its root and siblings (graph.go).
func (s *Store) ReapExpired(ctx context.Context) (reaped []Job, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE jobs
			SET state            = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			    last_error       = format('lease expired: worker %s stopped heartbeating (fence %s, attempt %s of %s)',
			                              lease_owner, fence, attempts, max_attempts),
			    available_at     = now() + `+backoffSQL+`,
			    progress         = 0,
			    lease_owner      = NULL,
			    lease_expires_at = NULL,
			    updated_at       = now()
			WHERE state IN ('running', 'validating') AND lease_expires_at < now()
			RETURNING `+columns)
		if err != nil {
			return err
		}
		reaped, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scan(r) })
		if err != nil {
			return err
		}
		return failDependents(ctx, tx)
	})
	return reaped, err
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM jobs WHERE id = $1`, id))
}

// List returns the most recently created jobs, newest first.
func (s *Store) List(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM jobs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scan(r) })
}

func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (Job, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM jobs WHERE idempotency_key = $1`, key))
}
