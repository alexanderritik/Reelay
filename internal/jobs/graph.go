package jobs

// The job graph for chunked proxies.
//
//	root (kind proxy) ──Plan──▶ waiting ──last child commits──▶ queued ──▶ finalize ──▶ done
//	   ├─ audio  (kind audio, highest priority)
//	   ├─ chunk 0
//	   ├─ …                      each child: queued → running → validating → done
//	   └─ chunk N-1              (ordinary jobs: leases, heartbeats, fences, retries)
//
// Fan-out: Plan inserts every child and parks the root in one transaction,
// fenced on the root. Fan-in: each child's commit decrements the root's
// pending_deps in the same transaction as the fenced commit, so the counter
// drops exactly once per child however often the child was retried, and
// exactly one commit takes it to zero and re-queues the root. If a child
// fails for good, the root and every unfinished sibling fail with it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/alexanderritik/reelay/internal/hls"
)

type Kind string

const (
	KindProxy Kind = "proxy" // an upload's job; the root of a chunked graph
	KindChunk Kind = "chunk" // one time range of the video
	KindAudio Kind = "audio" // the whole audio track, encoded once
)

// Claim priorities: higher is claimed first.
const (
	PriorityUpload = 0 // new uploads
	PriorityChild  = 1 // chunks, and a root's finalize step, so started jobs finish first
	PriorityAudio  = 2 // playback cannot start without audio
)

// ChunkSpec is one planned video chunk: the half-open source range
// [Start, End) in seconds and the number of source frames inside it.
type ChunkSpec struct {
	Start, End float64
	Frames     int
}

// Plan fans a running root out into chunk jobs (and an audio job if
// withAudio) and parks the root in waiting until they all commit. It is one
// transaction fenced on the root: a worker that lost the root gets
// ErrLeaseLost and nothing is inserted, and a crash mid-plan leaves nothing
// behind, so the next owner plans from scratch.
func (s *Store) Plan(ctx context.Context, rootID string, fence int64, chunks []ChunkSpec, withAudio bool) error {
	if len(chunks) == 0 {
		return errors.New("plan: no chunks")
	}
	deps := len(chunks)
	if withAudio {
		deps++
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var sourceKey string
		err := tx.QueryRow(ctx, `
			UPDATE jobs
			SET state            = 'waiting',
			    chunks           = $3,
			    pending_deps     = $4,
			    priority         = $5,
			    progress         = 0,
			    lease_owner      = NULL,
			    lease_expires_at = NULL,
			    updated_at       = now()
			WHERE id = $1 AND fence = $2 AND state = 'running' AND kind = 'proxy'
			RETURNING source_key`,
			rootID, fence, len(chunks), deps, PriorityChild).Scan(&sourceKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}

		rows := make([][]any, 0, deps)
		for i, c := range chunks {
			rows = append(rows, []any{uuid.NewString(), KindChunk, rootID, sourceKey,
				i, c.Start, c.End, c.Frames, PriorityChild})
		}
		if withAudio {
			rows = append(rows, []any{uuid.NewString(), KindAudio, rootID, sourceKey,
				nil, nil, nil, nil, PriorityAudio})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"jobs"},
			[]string{"id", "kind", "parent_id", "source_key",
				"chunk_index", "range_start", "range_end", "expected_frames", "priority"},
			pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("insert children: %w", err)
		}
		return nil
	})
}

// CommitChild marks a validated chunk or audio job done with what it
// published, and in the same transaction counts it off its root. Only the
// holder of the current fence can commit, and a job commits at most once, so
// each child decrements pending_deps exactly once. The commit that reaches
// zero moves the root from waiting to queued with fresh attempts, ready for
// its finalize step. A stale worker gets ErrLeaseLost and changes nothing.
func (s *Store) CommitChild(ctx context.Context, id string, fence int64, outputKey string, out hls.Output) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var parentID string
		err := tx.QueryRow(ctx, `
			UPDATE jobs
			SET state            = 'done',
			    output_key       = $3,
			    hls              = $4,
			    progress         = 100,
			    lease_owner      = NULL,
			    lease_expires_at = NULL,
			    updated_at       = now()
			WHERE id = $1 AND fence = $2 AND state = 'validating' AND kind <> 'proxy'
			RETURNING parent_id`,
			id, fence, outputKey, out).Scan(&parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}

		// The row lock on the root serializes concurrent decrements, and the
		// CASE sees the value this statement starts from, so exactly one
		// commit observes pending_deps = 1. A root that already failed (a
		// sibling gave up) is still counted down but never re-queued.
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			SET pending_deps = pending_deps - 1,
			    state        = CASE WHEN state = 'waiting' AND pending_deps = 1 THEN 'queued' ELSE state END,
			    attempts     = CASE WHEN state = 'waiting' AND pending_deps = 1 THEN 0 ELSE attempts END,
			    available_at = CASE WHEN state = 'waiting' AND pending_deps = 1 THEN now() ELSE available_at END,
			    updated_at   = now()
			WHERE id = $1`, parentID)
		return err
	})
}

// failDependents spreads permanent failure through the graph, inside the
// caller's transaction: a waiting root with a failed child fails with that
// child's reason, then every unfinished child of a failed root fails too.
// A sibling that is mid-transcode notices on its next heartbeat (its row is
// no longer running), and its ffmpeg is killed.
func failDependents(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		UPDATE jobs AS root
		SET state      = 'failed',
		    last_error = format('%s failed: %s',
		                        CASE WHEN c.kind = 'chunk' THEN 'chunk ' || c.chunk_index ELSE c.kind END,
		                        c.last_error),
		    updated_at = now()
		FROM jobs AS c
		WHERE c.parent_id = root.id AND c.state = 'failed' AND root.state = 'waiting'`)
	if err != nil {
		return fmt.Errorf("fail roots: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE jobs AS c
		SET state            = 'failed',
		    last_error       = 'cancelled: ' || root.last_error,
		    progress         = 0,
		    lease_owner      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = now()
		FROM jobs AS root
		WHERE c.parent_id = root.id AND root.state = 'failed'
		  AND c.state IN ('queued', 'running', 'validating')`)
	if err != nil {
		return fmt.Errorf("cancel siblings: %w", err)
	}
	return nil
}

// Children returns a root's children: video chunks in order, then audio.
func (s *Store) Children(ctx context.Context, rootID string) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM jobs WHERE parent_id = $1
		ORDER BY kind = 'audio', chunk_index`, rootID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scan(r) })
}

// ListRoots returns the most recent upload jobs, newest first, leaving out
// their chunk and audio jobs.
func (s *Store) ListRoots(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM jobs WHERE kind = 'proxy' ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scan(r) })
}
