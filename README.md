# Reelay

**Add video to your app in an afternoon.**

Upload a file and get back a link that plays smoothly on any device. Reelay runs on your own servers, and it never loses a video: kill a worker in the middle of encoding and the video still finishes.

```bash
curl -F file=@video.mp4 localhost:8080/videos
# → { "id": "3f0c…", "status": "processing", ... }

curl localhost:8080/videos/3f0c…
# → { "status": "ready", "play_url": ".../videos/3f0c…/hls/master.m3u8", "thumbnail_url": "..." }
```

<!-- GIF: upload → kill a worker → the video still finishes → plays. Record from the dashboard + `./reelay chaos`. -->

## Why it exists

- **Crash-safe processing.** Every encode step holds a lease and a fencing token in Postgres. A worker that dies or freezes is replaced, and its late writes are rejected. A suite of 14 failure scenarios runs real workers against real Postgres and ffmpeg to prove it ([docs/ENGINE.md](docs/ENGINE.md)).
- **Plays while it encodes.** Long videos are split into chunks that encode in parallel. Playback starts as soon as the first chunk is done.
- **Every output is checked before it counts.** Frame counts, keyframes, seams and audio length are verified before a video is marked ready.
- **Your servers, one command.** `docker compose up` starts the API, the dashboard, workers and Postgres.

## Status

Reelay is early. What works today and what is next:

| | |
|---|---|
| ✅ Upload API with idempotent retries | ✅ Dashboard: upload, status, live chunk map, player, embed snippets |
| ✅ HLS playback while processing (long videos) | ✅ Embeddable player at `/embed/{id}` |
| ✅ Poster thumbnails | ✅ `docker compose up` |
| 🔜 Resumable uploads (tus) | 🔜 360p / 720p / 1080p adaptive ladder |
| 🔜 Webhooks on ready / failed | 🔜 Hover previews (storyboard) |
| 🔜 Auto captions (Whisper) | 🔜 Workers on many machines, GPU encoding |

The full roadmap is in [PLAN.md](PLAN.md).

## Run it

```bash
docker compose up --build        # dashboard and API on http://localhost:8080
```

For development, run only the database in Docker:

```bash
docker compose up -d postgres    # Postgres on :5435
go run . serve                   # API and dashboard on :8080
go run . worker                  # run as many as you like
```

Needs Go 1.26+ and ffmpeg on the machine that runs workers.

## API

| Endpoint | What it does |
|---|---|
| `POST /videos` | Upload (multipart `file`, optional `title`). Send `Idempotency-Key` so a retried upload never makes a duplicate. Unreadable files are rejected with 422 right away. |
| `GET /videos` | List, newest first |
| `GET /videos/{id}` | Status, progress, `play_url`, and the per-chunk map |
| `PATCH /videos/{id}` | Rename: `{"title": "…"}` |
| `DELETE /videos/{id}` | Delete a ready or failed video and its files |
| `GET /videos/{id}/hls/master.m3u8` | HLS playback (long videos) |
| `GET /videos/{id}/video.mp4` | MP4 playback (short videos; HLS for everything is planned) |
| `GET /videos/{id}/thumbnail.jpg` | Poster frame, available right after upload |
| `GET /embed/{id}` | A player page for an `<iframe>` |

A video's `status` is `processing`, `ready` or `failed`. `playable` turns true as soon as `play_url` works, which for long videos is well before `ready`. A failed video has an `error` saying why.

Settings (environment variables): `DATABASE_URL`, `DATA_DIR`, `ADDR`, `PUBLIC_URL` (base for returned URLs behind a proxy), and the worker settings in [docs/ENGINE.md](docs/ENGINE.md#running-it).

## Honest limits

- No authentication yet. Run it on a private network or behind your own auth proxy.
- Storage is local disk, so the API and all workers must share one volume (one machine, or a shared mount). S3-compatible storage is planned.
- One output rendition today (540p H.264). The adaptive ladder is next.
- Throughput numbers ("good for up to X videos a day on one machine") come after the claim benchmark in [PLAN.md](PLAN.md).

## How it is built

```
Reelay (product)                    Conduit (engine)
────────────────────                ─────────────────────────────────────
internal/videos   /videos API  ──►  internal/jobs     job table: claim, lease, fence, retry
internal/web      dashboard,        internal/worker   claim loop, heartbeats, chunk fan-out
                  embed player      internal/media    ffmpeg/ffprobe, validation
                                    internal/hls      playlists
                                    internal/storage  blob storage
```

The product only reads the engine's job table and creates jobs through it, so the engine's guarantees hold for everything the API does. [docs/ENGINE.md](docs/ENGINE.md) explains the state machine, fencing, chunking and the failure tests.

```bash
go test ./...          # needs Postgres (docker compose up -d postgres) and ffmpeg
go run ./cmd/chaos     # the 14 failure scenarios against real workers
go run . chaos         # watch them in a browser console on 127.0.0.1:8090
```
