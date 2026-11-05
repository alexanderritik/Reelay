// Package videos is the product layer on top of the job engine: an upload
// becomes a video with a title, a status a person understands, and URLs to
// play it. Processing state is read from the engine's jobs table and never
// written here, so every crash-safety rule in jobs/ still holds.
package videos

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/jobs"
)

//go:embed schema.sql
var schema string

// migrateLockID serializes this schema across processes that start together,
// like db.Migrate does for the engine's.
const migrateLockID = 7_316_003

// Migrate applies the videos schema. It needs the jobs table, so it runs
// after db.Migrate.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("migrate videos: %w", err)
	}
	return nil
}

var (
	ErrNotFound = errors.New("video not found")
	ErrBusy     = errors.New("video is still processing")
)

// Status is what the API reports: the engine's six job states collapsed to
// the three a developer acts on.
type Status string

const (
	StatusProcessing Status = "processing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
)

// row is one root job joined with its metadata and a summary of its children.
type row struct {
	ID, Title, Filename string
	SizeBytes           int64
	Duration            *float64
	SourceHeight        *int
	State               jobs.State
	Progress            float64
	LastError           *string
	Attempts            int
	Chunks              *int
	OutputKey           *string
	CreatedAt           time.Time
	UpdatedAt           time.Time

	Children      int
	ChildrenDone  int
	ChildProgress float64
	FirstChunkOK  bool     // chunk 0 committed
	AudioOK       bool     // audio committed, or the source has none
	Renditions    []string // rung names chunk 0 published, lowest first
}

// selectRows reads root jobs. A root without a videos row (an upload made
// through the engine's /jobs API, or a crash between the two inserts) still
// shows up, untitled, rather than disappearing.
const selectRows = `
	SELECT j.id, COALESCE(v.title, ''), COALESCE(v.filename, ''), COALESCE(v.size_bytes, 0),
	       v.duration_seconds, v.source_height,
	       j.state, j.progress, j.last_error, j.attempts, j.chunks, j.output_key, j.created_at, j.updated_at,
	       c.total, c.done, c.progress, c.first_ok, c.audio_ok, COALESCE(c.renditions, '[]'::jsonb)
	FROM jobs j
	LEFT JOIN videos v ON v.id = j.id
	LEFT JOIN LATERAL (
		SELECT count(*)                                         AS total,
		       count(*) FILTER (WHERE k.state = 'done')         AS done,
		       COALESCE(avg(k.progress), 0)                     AS progress,
		       COALESCE(bool_or(k.kind = 'chunk' AND k.chunk_index = 0 AND k.state = 'done'), false) AS first_ok,
		       COALESCE(bool_and(k.state = 'done') FILTER (WHERE k.kind = 'audio'), true)           AS audio_ok,
		       -- Only the rung names, not chunk 0's whole segment list.
		       (array_agg(jsonb_path_query_array(k.hls, '$.renditions[*].name'))
		            FILTER (WHERE k.kind = 'chunk' AND k.chunk_index = 0 AND k.state = 'done'))[1]   AS renditions
		FROM jobs k WHERE k.parent_id = j.id
	) c ON true
	WHERE j.kind = 'proxy'`

func scanRow(r pgx.Row) (row, error) {
	var v row
	err := r.Scan(&v.ID, &v.Title, &v.Filename, &v.SizeBytes, &v.Duration, &v.SourceHeight,
		&v.State, &v.Progress, &v.LastError, &v.Attempts, &v.Chunks, &v.OutputKey, &v.CreatedAt, &v.UpdatedAt,
		&v.Children, &v.ChildrenDone, &v.ChildProgress, &v.FirstChunkOK, &v.AudioOK, &v.Renditions)
	if errors.Is(err, pgx.ErrNoRows) {
		return row{}, ErrNotFound
	}
	return v, err
}

func (v row) status() Status {
	switch v.State {
	case jobs.StateDone:
		return StatusReady
	case jobs.StateFailed:
		return StatusFailed
	}
	return StatusProcessing
}

// progress is one number for the whole upload. A chunked job's root sits at
// 0 while its children work, so its progress is their average, held below
// 100 until the root's final check commits.
func (v row) progress() float64 {
	switch {
	case v.State == jobs.StateDone:
		return 100
	case v.Chunks == nil:
		return v.Progress
	}
	return min(99, v.ChildProgress)
}

// playback reports how the video plays (HLS, once planned into chunks) and
// whether a player can start on it yet, which can be long before it is ready.
// Every upload is chunked, so there is no other format; a root that took the
// engine's single-file path (a worker run with CHUNK_MIN_DURATION set) has no
// playback through the product API.
func (v row) playback() (format string, playable bool) {
	if v.Chunks == nil {
		return "", false
	}
	return "hls", v.State != jobs.StateFailed && v.FirstChunkOK && v.AudioOK
}

// Meta is what the API records about an upload besides its job.
type Meta struct {
	Title, Filename string
	SizeBytes       int64
	Duration        float64
	SourceHeight    int
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Insert records metadata for a root job created by the caller. A second
// insert for the same job (a retried upload) keeps the first.
func (s *Store) Insert(ctx context.Context, id string, m Meta) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO videos (id, title, filename, size_bytes, duration_seconds, source_height)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0::float8), NULLIF($6, 0))
		ON CONFLICT (id) DO NOTHING`,
		id, m.Title, m.Filename, m.SizeBytes, m.Duration, m.SourceHeight)
	return err
}

func (s *Store) list(ctx context.Context, limit int) ([]row, error) {
	rows, err := s.pool.Query(ctx, selectRows+` ORDER BY j.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) { return scanRow(r) })
}

func (s *Store) get(ctx context.Context, id string) (row, error) {
	return scanRow(s.pool.QueryRow(ctx, selectRows+` AND j.id = $1`, id))
}

// Rename changes a video's title, creating its metadata row if it had none.
func (s *Store) Rename(ctx context.Context, id, title string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO videos (id, title)
		SELECT id, $2 FROM jobs WHERE id = $1 AND kind = 'proxy'
		ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title`, id, title)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a finished or failed video's jobs and metadata. A video
// still processing is refused with ErrBusy: its workers hold leases, and
// removing the rows under them would only turn their commits into errors.
// The root's row lock keeps a concurrent claim from starting it meanwhile.
func (s *Store) Delete(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var state jobs.State
		err := tx.QueryRow(ctx, `SELECT state FROM jobs WHERE id = $1 AND kind = 'proxy' FOR UPDATE`, id).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != jobs.StateDone && state != jobs.StateFailed {
			return ErrBusy
		}
		if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE parent_id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, id) // cascades to videos
		return err
	})
}
