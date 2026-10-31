-- Conduit job table. The CHECK constraints encode the state-machine invariants,
-- so a buggy worker cannot write an impossible row even if the Go code is wrong.

CREATE TABLE IF NOT EXISTS jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Invariant 1: exactly one state at a time.
    state            TEXT NOT NULL DEFAULT 'queued'
                     CONSTRAINT jobs_state_check
                     CHECK (state IN ('queued', 'running', 'validating', 'waiting', 'done', 'failed')),

    source_key       TEXT NOT NULL,
    output_key       TEXT,

    -- Invariant 3: incremented on every claim and never reset; every worker
    -- write is guarded by it. Ownership only, not retry accounting.
    fence            BIGINT NOT NULL DEFAULT 0,

    -- Incremented on every claim, so a worker crash (lease expiry) consumes an
    -- attempt just like an ffmpeg or validation failure. Unlike fence, this may
    -- be reset (e.g. a manual retry) without weakening fencing.
    attempts         INT    NOT NULL DEFAULT 0,
    max_attempts     INT    NOT NULL DEFAULT 3,

    lease_owner      TEXT,
    lease_expires_at TIMESTAMPTZ,

    last_error       TEXT,
    idempotency_key  TEXT UNIQUE,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT attempts_capped
        CHECK (attempts BETWEEN 0 AND max_attempts),

    -- Invariant 4: a done job always points at an output file.
    CONSTRAINT done_has_output
        CHECK (state <> 'done' OR output_key IS NOT NULL),

    -- Invariant 5: a job being worked on always has a lease that can expire.
    CONSTRAINT active_has_lease
        CHECK (state NOT IN ('running', 'validating')
               OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL))
);

-- Retry backoff: a queued job is not claimable before available_at.
-- Added as an ALTER so existing databases pick it up in place.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS
    available_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Transcode progress of the current attempt, 0-100. Written by the owning
-- worker (fenced), reset when an attempt starts over, 100 only once done.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS
    progress REAL NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100);

-- ---------------------------------------------------------------------------
-- Chunked proxies. A long source is split into a job graph: the upload's
-- job (kind 'proxy') plans one 'chunk' job per time range plus one 'audio'
-- job, waits for them in state 'waiting', then runs once more to validate
-- the assembled result. Short sources never get children and take the
-- single-file path unchanged.
-- ---------------------------------------------------------------------------

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'proxy';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS parent_id UUID REFERENCES jobs (id);
-- Chunk jobs only: position, the half-open source time range [start, end) in
-- seconds, and how many source frames fall in it (checked after encoding).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS chunk_index INT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS range_start DOUBLE PRECISION;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS range_end DOUBLE PRECISION;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS expected_frames INT;
-- Claim order: higher first. Children outrank new uploads so started jobs
-- finish first, and audio outranks chunks because playback needs it.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS priority INT NOT NULL DEFAULT 0;
-- Root only: number of video chunks once planned (NULL = not planned), and
-- how many children have not committed yet. The last child's commit takes
-- pending_deps to 0 and re-queues the root, in the same transaction.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS chunks INT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS pending_deps INT NOT NULL DEFAULT 0;
-- Children only: the HLS segments the commit published.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS hls JSONB;

-- Constraints are added once, guarded by name, so concurrent startups do not
-- take table locks on every boot.
DO $$
BEGIN
    -- Invariant 1, extended with 'waiting': a root whose children are running.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'jobs_state_check'
                     AND pg_get_constraintdef(oid) LIKE '%waiting%') THEN
        ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_state_check;
        ALTER TABLE jobs ADD CONSTRAINT jobs_state_check
            CHECK (state IN ('queued', 'running', 'validating', 'waiting', 'done', 'failed'));
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'kind_valid') THEN
        ALTER TABLE jobs ADD CONSTRAINT kind_valid
            CHECK (kind IN ('proxy', 'chunk', 'audio'));
    END IF;

    -- Chunk and audio jobs always belong to a root; a root never has a parent.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'child_has_parent') THEN
        ALTER TABLE jobs ADD CONSTRAINT child_has_parent
            CHECK ((kind = 'proxy') = (parent_id IS NULL));
    END IF;

    -- A chunk always knows its range and expected frame count.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chunk_has_range') THEN
        ALTER TABLE jobs ADD CONSTRAINT chunk_has_range
            CHECK (kind <> 'chunk' OR (chunk_index IS NOT NULL AND range_start < range_end
                                       AND expected_frames > 0));
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pending_deps_valid') THEN
        ALTER TABLE jobs ADD CONSTRAINT pending_deps_valid CHECK (pending_deps >= 0);
    END IF;

    -- A waiting root is owed at least one commit and holds no lease: no worker
    -- is on it, so the reaper never touches it.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'waiting_is_idle') THEN
        ALTER TABLE jobs ADD CONSTRAINT waiting_is_idle
            CHECK (state <> 'waiting' OR (pending_deps > 0 AND lease_owner IS NULL));
    END IF;

    -- A committed child always says what it published.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'done_child_has_hls') THEN
        ALTER TABLE jobs ADD CONSTRAINT done_child_has_hls
            CHECK (state <> 'done' OR kind = 'proxy' OR hls IS NOT NULL);
    END IF;
END $$;

-- Backstop for planning: a root can never get two chunks with one index.
CREATE UNIQUE INDEX IF NOT EXISTS jobs_chunk_unique ON jobs (parent_id, chunk_index)
    WHERE kind = 'chunk';
CREATE UNIQUE INDEX IF NOT EXISTS jobs_audio_unique ON jobs (parent_id) WHERE kind = 'audio';
CREATE INDEX IF NOT EXISTS jobs_parent_idx ON jobs (parent_id) WHERE parent_id IS NOT NULL;

-- Workers claim the highest-priority, oldest queued job; this keeps that scan
-- cheap. Replaces the created_at-only index from before priorities existed.
DROP INDEX IF EXISTS jobs_queued_idx;
CREATE INDEX IF NOT EXISTS jobs_claim_idx ON jobs (priority DESC, created_at, chunk_index)
    WHERE state = 'queued';

-- The reaper looks for active jobs whose lease ran out.
CREATE INDEX IF NOT EXISTS jobs_lease_idx ON jobs (lease_expires_at)
    WHERE state IN ('running', 'validating');
