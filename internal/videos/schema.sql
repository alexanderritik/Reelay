-- Product metadata for an upload. The engine's jobs table stays the source
-- of truth for processing state; this table only adds what a person sees.
-- One row per root job, removed with it.

CREATE TABLE IF NOT EXISTS videos (
    id               UUID PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    title            TEXT NOT NULL,
    filename         TEXT NOT NULL DEFAULT '',
    size_bytes       BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    duration_seconds DOUBLE PRECISION,
    source_height    INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
