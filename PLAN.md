# Reelay plan

*Last updated: 31 Oct 2026*

## The product in one paragraph

Reelay is a self-hosted video API. A developer uploads a video and gets back a link that plays smoothly in any browser, on any connection. They never touch ffmpeg, queues or HLS, and a video is never lost, even when servers crash. It is built on Conduit, a crash-safe job engine for video, which already handles the hard part.

**Pitch:** "Upload any video, get a link that plays everywhere, on your own servers, and never lose a video, even when things crash."

**For:**
- Developers building course platforms, learning apps and community sites who need video but are not video experts.
- Teams with private video that must stay on their own servers: hospitals, schools, law firms, internal training, and countries with data-residency laws.
- Indie developers who want video without a bill that grows with every view.

**Words to use:** own your video, never loses a video, plays everywhere, simple API.
**Avoid:** "free alternative to…", "cheaper than…", "clone of…", "enterprise-grade".

## Architecture

```
Reelay (product)                       Conduit (engine)
───────────────────                    ───────────────────────────────
/videos API, uploads,      ──────►     jobs table: claim / lease / fence
webhooks, dashboard,                   chunked fan-out and fan-in
ladder config, captions                ffmpeg, validation, HLS, crash safety
```

**Rule:** the product never writes the engine's state columns. It creates jobs through `jobs.Store` and reads the jobs table. Product data lives in its own tables (`videos`, later `uploads`, `webhook_events`), so the engine's invariants hold whatever the product does.

**Importing Conduit as a module.** The goal is `go get github.com/alexanderritik/crash-safe-proxy-pipeline`. That does not work today, because Go forbids importing another module's `internal/` packages. The steps:

1. Now: the engine is a copy in this repo (`internal/jobs`, `worker`, `media`, `hls`, `storage`, `db`, `api`). The product code (`internal/videos`, `internal/web`) only touches it through exported APIs.
2. After the quality ladder lands, the engine's API will have settled. Then move the engine packages in the Conduit repo out of `internal/` (for example `engine/jobs`), tag `v0.1.0`, import it here and delete the copies.
3. Engine changes then go to Conduit first, with its chaos suite, and the product picks them up through version bumps.

The ladder changes the engine heavily (rendition specs, the per-chunk output format, master playlists), so extracting before it would mean publishing an API that changes immediately.

## What was cleaned up (31 Oct 2026)

| Removed | Why |
|---|---|
| `data/` (11 GB) | Copies of sample videos and old outputs; the originals stay in `../Conduit/data` |
| `conduit` binary, `v.mp4`, `chaos.log` | Build output and local test files |
| `memory.md` | Private brief for the Cardboard application, not part of this product |
| `cmd/bench`, `bench/seek` | A seek and scrub benchmark for a video *editor* (Playwright, 4K Netflix footage). It does not measure what this product needs; the claim benchmark below replaces it |
| Git history | Fresh repository; Conduit's history stays in its own repo |

Renamed: module `github.com/alexanderritik/reelay`, binary `reelay`, database `reelay` on port 5435, container `reelay-postgres`.
Kept as engine names: `CONDUIT_CHAOS` fault flags, the chaos console and the chaos suite. They are the proof behind "never loses a video".

## Phase 0: Foundation ✅ (done)

- [x] Clean the repo, fresh git history, product naming
- [x] `/videos` API: upload with an idempotency key and early rejection of unreadable files, list, get with the chunk map, rename, delete (refused while processing), thumbnail, MP4 and HLS playback
- [x] Dashboard: drag-and-drop upload with progress, cancel and retry; stats; filter and search; live status; video page with player, processing steps, a live chunk map that shows retried chunks, embed/HTML/API snippets, rename, delete; API guide; light and dark themes; works at phone width
- [x] Embeddable player at `/embed/{id}`; hls.js is built into the binary, so nothing loads from a CDN
- [x] `docker compose up`: Postgres, API and two workers
- [x] Verified end to end: a worker was killed with `kill -9` mid-video, the part was reclaimed, the video finished, and the dashboard showed "1 part retried"

## Phase 1: Version 1 (in this order)

Each item lists what "done" means. Engine changes keep the chaos suite green and add a scenario where they add a failure mode.

### 1. HLS for every video
Short videos take the single-file MP4 path today, so `play_url` is sometimes `.mp4`. Send every video through the chunked path (one chunk for short videos) and remove the MP4 branch from the product.
**Done when:** every `play_url` is a `master.m3u8`, and a 3-second clip and a 2-hour file both play.

### 2. Quality ladder: 360p, 720p, 1080p *(engine)*
- Each chunk job encodes **all rungs in one ffmpeg run** (decode once, `split` filter, one output per rung). This costs less than one job per rung per chunk, and a retry still redoes only one chunk.
- Never upscale: rungs above the source height are skipped. A 720p source gets 360p and 720p.
- Capped CRF per rung (`-crf 23 -maxrate/-bufsize`): roughly 360p at 0.8 Mbps, 720p at 2.8 Mbps, 1080p at 5 Mbps.
- Storage: `proxies/<root>/v/<rung>/<chunk>/<fence>/…`. The child's `hls` column becomes a map from rung to media.
- Master playlist: one `EXT-X-STREAM-INF` per rung with `RESOLUTION`, a `BANDWIDTH` measured from the real segments, and `CODECS`.
- Validation checks every rung (frames, keyframes, `tfdt`). A missing or short rung fails the chunk.
- Ladder settings live in the product config and are passed to the engine as a list of rendition specs.
**Done when:** hls.js switches rungs under DevTools throttling, and a chaos scenario that truncates one rung is caught.

### 3. Resumable uploads (tus)
- Use tusd's handler (`github.com/tus/tusd/v2/pkg/handler`) with its file store under `DATA_DIR/uploads`, mounted at `/uploads`. Do not hand-roll the protocol.
- When an upload completes, run the same probe and job creation as `POST /videos`, using the tus upload id as the idempotency key, so a repeated completion hook cannot create two videos.
- The dashboard switches to tus-js-client, vendored like hls.js. Uploads survive a page reload, using the fingerprint in `localStorage`.
- Keep multipart `POST /videos` for small files and curl.
**Done when:** a 2 GB upload resumes from its offset after (a) the network drops at 80% and (b) the API restarts mid-upload.

### 4. Thumbnails and preview strip
- A `thumbs` child job, fanned out next to `audio`, makes the poster, a sprite sheet (one 160 px tile every *N* seconds, with *N* chosen so a video has at most about 100 tiles) and `storyboard.vtt` with `#xywh=` cues.
- Drop the API's on-demand ffmpeg run. The poster is still made from the source first, so it shows right after upload.
- The dashboard player shows hover previews on the timeline.
**Done when:** hovering anywhere on a 1-hour video shows the right frame within a tile.

### 5. Webhooks
- Configuration: `WEBHOOK_URL` and `WEBHOOK_SECRET` (env vars first; a settings page later).
- **Transactional outbox:** a Postgres trigger on `jobs` (a root moving to `done` or `failed`) inserts a `webhook_events` row in the same transaction as the engine's commit. No event can be lost and no hook is needed in engine code.
- A delivery loop claims events with `FOR UPDATE SKIP LOCKED` (the same pattern as jobs) and retries with jittered backoff for 24 h. Each request is signed with `Reelay-Signature: t=<ts>,v1=<hmac-sha256>` and carries an event id that receivers use to drop duplicates.
- Events: `video.ready`, `video.failed`. Later: `video.playable` (first chunk is in).
- The dashboard gets a delivery log per video and a "Redeliver" button.
**Done when:** a receiver that is down for 5 minutes gets every event once it is back, and kill tests show no lost events.

### 6. Retry a failed video
`POST /videos/{id}/retry`: in one transaction, reset `attempts` on the failed root and its failed children and set them back to `queued`. Fences are never reset. Add a "Retry" button in the dashboard.

### 7. Dashboard polish for V1
Ladder badges (which rungs exist), a webhook log, a retry button, hover previews, errors grouped by reason, and a "copy curl" for each video.

### 8. Launch kit
- The GIF: upload → `kill -9` a worker → the video still finishes → it plays. Record the dashboard and `./reelay chaos` side by side.
- A "Why we built this" post: "I built Conduit, saw videos getting lost in crashes, and made this so nobody else has to solve it again."
- `.env.example`, a section on putting it behind Caddy or nginx with TLS, and the honest-limits section filled in with benchmark numbers.

## Phase 2: Scale and captions

### 9. Object storage (S3 / MinIO)
Workers on many machines need shared storage. Add an S3 implementation of `storage.Storage`, with pre-signed range reads so a chunk worker fetches only its own byte range. Add MinIO to `docker-compose.yml` as an optional profile.

### 10. Workers on many machines, including GPUs
- Add a `requires TEXT[]` column on jobs. Workers set `CAPABILITIES=gpu,nvenc`, and `Claim` adds `AND requires <@ $caps`. CPU workers never take GPU-only jobs; GPU workers may take anything.
- Add an NVENC encoder profile (`h264_nvenc`, B-frames off, pinned profile and level) and teach validation to accept its output. Benchmark it against libx264 per rung.
- Show worker liveness in the dashboard: worker ids are already in `lease_owner`, so add a heartbeat table.

### 11. Claim benchmark
This does not exist yet; the old `cmd/bench` measured editor seeking. Write `cmd/bench-claim`: spin up *N* simulated workers (10 → 500) with sleep jobs against one Postgres, and measure claims per second, p50 and p99 claim latency, reaper cost and lock waits. Find where `SKIP LOCKED` contention or the claim index stops scaling.
**Output:** the README line "Good for up to X videos a day on one machine, Y workers per Postgres".

### 12. Auto captions with Whisper
- A separate `captions` root job, not a child, so `ready` never waits on captions. It shows as `captions: pending | ready | failed` on the video.
- Use whisper.cpp (CPU, `base` model) or faster-whisper on GPU workers (`requires = {gpu}`). Output WebVTT with language detection.
- Add an HLS `SUBTITLES` group in the master playlist and a `<track>` in the embed player.
**Done when:** a 10-minute talk gets captions you can read, and killing the captions worker mid-run still ends in captions.

### 13. Metrics
`/metrics` for Prometheus: queue depth by kind, claim latency, encode seconds per minute of video, failures by reason, webhook backlog.

## Phase 3: Production readiness

- API keys (stored hashed, with scopes) and a dashboard login. Until then the dashboard is for trusted networks only.
- Signed playback URLs (HMAC with an expiry) for private video, and a CORS setting for players on other domains.
- Upload limits and rate limiting per key.
- Cache rendered playlists for high request rates.
- Storage lifecycle: optionally delete sources after *N* days, and recover orphaned files.

## Proof that it is a real product

1. **One real user** besides you, such as a friend's course site. One real user beats 100 features.
2. The "Why we built this" story.
3. Honest limits in the README, with numbers from the claim benchmark.

## Open decisions

| Decision | Recommendation |
|---|---|
| Name | "Reelay" is a working name. Renaming means changing the module path and replacing `reelay` across the repo |
| Ladder: one job per chunk for all rungs, or one job per chunk per rung | One job per chunk (decode once). Revisit once GPU workers exist |
| tus: library or hand-rolled | tusd's handler |
| License | MIT for the most adoption, or AGPL-3.0 to stop a closed hosted fork. Decide before the first public post |
