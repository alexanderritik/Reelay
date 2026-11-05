# Reelay plan

*Last updated: 6 Nov 2026*

## The product in one paragraph

Reelay is a self-hosted video API. A developer uploads a video and gets back a link that plays smoothly in any browser, on any connection. They never touch ffmpeg, queues or HLS, and a video is never lost, even when servers crash. It is built on Conduit, a crash-safe job engine for video, which already handles the hard part.

**Pitch:** "Upload any video, get a link that plays everywhere, on your own servers, and never lose a video, even when things crash."

**First customer (decided 6 Nov 2026):** course and edtech platforms: indie course creators, small LMS builders and learning apps. The pitch to them is **ownership and a flat cost**: their videos stay on their own servers, and the bill does not grow with every view. The README, the demo and the first outreach speak to this customer only.

**Later audiences**, once the first one works:
- Teams with private video that must stay on their own servers: hospitals, schools, law firms, internal training, and countries with data-residency laws.
- AI products that need transcripts and search over video. This builds on captions (Phase 2) and is the follow-up story, not a pivot.

**Words to use:** own your video, never loses a video, plays everywhere, simple API, flat cost.
**Avoid:** "free alternative to…", "cheaper than…", "clone of…", "enterprise-grade".

## Architecture

```
Reelay (product)                       Conduit (engine)
───────────────────                    ───────────────────────────────
/videos API, uploads,      ──────►     jobs table: claim / lease / fence
webhooks, dashboard,                   chunked fan-out and fan-in
ladder config, captions                ffmpeg, validation, HLS, crash safety
```

**Rule:** the product never writes the engine's state columns. It creates jobs through `jobs.Store` and reads the jobs table. Product data lives in its own tables (`videos`, later `webhook_events`), so the engine's invariants hold whatever the product does.

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

## Phase 1: First real user (in this order)

The goal of this phase is a public demo and one course platform using Reelay. Only what that user needs is built here; everything else waits for their feedback (see "After the first users").

Each item lists what "done" means. Engine changes keep the chaos suite green and add a scenario where they add a failure mode.

### 1. HLS for every video ✅ (done 6 Nov 2026)
Short videos take the single-file MP4 path today ([internal/worker/chunked.go](internal/worker/chunked.go), `processUpload`), so `play_url` is sometimes `.mp4`. `PlanChunks` already gives a single chunk for a short clip, so no new engine logic is needed.
- **Engine:** keep `processSingle`. Most chaos scenarios run on the single-file path (`cmd/chaos/main.go` checks `proxies/<id>/<fence>.mp4`), and removing it would remove that proof. Instead, `CHUNK_MIN_DURATION=0` means "always chunk", and that becomes the product default in `main.go` and `docker-compose.yml`. The chaos harness sets its own env, so it keeps testing both paths. Porting those scenarios to the chunked path can come with the Conduit extraction.
- **Edge cases:** a 1–3 second clip (one chunk), a single-frame video, a clip with no audio. Add them to `internal/media/chunk_test.go`, plus an end-to-end check that a 3 s clip ends with a `master.m3u8`.
- **Product:** `play_url` is always HLS and `format` is always `"hls"`. Remove `GET /videos/{id}/video.mp4` and the MP4 branches in the dashboard (`internal/web/static/app.js`) and the README.
**Done when:** every `play_url` is a `master.m3u8`, a 3-second clip and a 2-hour file both play in `/embed/{id}`, and `go run ./cmd/chaos` passes.

### 2. Quality ladder: 360p, 720p, 1080p *(engine)* ✅ (done 6 Nov 2026)
- Each chunk job encodes **all rungs in one ffmpeg run** (decode once, `split` filter, one output per rung). This costs less than one job per rung per chunk, and a retry still redoes only one chunk.
- Never upscale: rungs above the source's short side are skipped. A 720p source gets 360p and 720p; a source between rungs (478p) also gets a rung at its own size. Portrait video is laddered by its short side.
- Capped CRF per rung (`-crf 23 -maxrate/-bufsize`): roughly 360p at 0.8 Mbps, 720p at 2.8 Mbps, 1080p at 5 Mbps.
- Storage: `proxies/<root>/v/<chunk>/<fence>/<rung>/…`, so every rung of a chunk sits under the chunk's own prefix: one fenced commit publishes them together and one prefix delete cleans up a failed attempt. The child's `hls` column lists the renditions.
- Master playlist: one `EXT-X-STREAM-INF` per rung with `RESOLUTION`, a `BANDWIDTH` measured from the real segments, and `CODECS`.
- Validation checks every rung (frames, keyframes, `tfdt`). A missing or short rung fails the chunk.
- Ladder settings: the `LADDER` env var (`360:800k,720:2800k,1080:5000k`), passed to the engine as rendition specs.
- Dashboard: show which rungs a video has.
**Done when:** hls.js switches rungs under DevTools throttling, and a chaos scenario that truncates one rung is caught.

### 3. Webhooks
- Configuration: `WEBHOOK_URL` and `WEBHOOK_SECRET` (env vars first; a settings page later).
- **Transactional outbox:** a Postgres trigger on `jobs` (a root moving to `done` or `failed`) inserts a `webhook_events` row in the same transaction as the engine's commit. No event can be lost and no hook is needed in engine code.
- A delivery loop claims events with `FOR UPDATE SKIP LOCKED` (the same pattern as jobs) and retries with jittered backoff for 24 h. Each request is signed with `Reelay-Signature: t=<ts>,v1=<hmac-sha256>` and carries an event id that receivers use to drop duplicates.
- Events: `video.ready`, `video.failed`. Later: `video.playable` (first chunk is in).
- The dashboard gets a delivery log per video. A "Redeliver" button waits until someone needs it.
**Done when:** a receiver that is down for 5 minutes gets every event once it is back, and kill tests show no lost events.

### 4. Minimal API key *(moved up from Phase 3)*
A public demo or a real user's server needs this before anything else goes online.
- One `API_KEY` env var. `POST`, `PATCH` and `DELETE /videos` need `Authorization: Bearer <key>`, compared in constant time. With no key set, the server refuses to start unless `INSECURE_NO_AUTH=1` is set, so nobody ships an open server by accident.
- Playback (`/videos/{id}/hls/…`, thumbnail) and `/embed/{id}` stay public. Video ids are random UUIDs, so links cannot be guessed.
- The dashboard asks for the key once and keeps it in `localStorage`.
- Hashed keys, scopes and several keys stay in Phase 3.
**Done when:** a write without the key gets 401, playback and embeds still work without it, and the README's curl examples include the header.

### 5. Public demo and launch
- **Demo:** a small VPS behind Caddy with TLS, `docker compose up`, the API key set. A shared demo key with an upload size limit and a nightly wipe, so strangers can try it. localhost doesn't count.
- **The GIF:** upload → `kill -9` a worker → the video still finishes → it plays. Record the dashboard and `./reelay chaos` side by side.
- **README for the first customer:** "Add video to your course platform in an afternoon." `.env.example`, a section on Caddy or nginx with TLS, and honest limits.
- **Posts:** "Why we built this" ("I built Conduit, saw videos getting lost in crashes, and made this so nobody else has to solve it again"), and a short write-up of the browser bug. Then Show HN and r/selfhosted.
- **Outreach:** a message under 80 words to course creators and LMS builders that names their video pipeline problem and links the demo.
**Done when:** one course platform other than yours is using Reelay, and you have talked to 10 possible users.

## After the first users (build when someone asks)

These were in Version 1 before. They are good features, but none of them is needed to win the first user. Build them in the order users ask for them.

### 6. Resumable uploads (tus)
- Use tusd's handler (`github.com/tus/tusd/v2/pkg/handler`) with its file store under `DATA_DIR/uploads`, mounted at `/uploads`. Do not hand-roll the protocol.
- When an upload completes, run the same probe and job creation as `POST /videos`, using the tus upload id as the idempotency key, so a repeated completion hook cannot create two videos.
- The dashboard switches to tus-js-client, vendored like hls.js. Uploads survive a page reload, using the fingerprint in `localStorage`.
- Keep multipart `POST /videos` for small files and curl.
**Done when:** a 2 GB upload resumes from its offset after (a) the network drops at 80% and (b) the API restarts mid-upload.

### 7. Thumbnails and preview strip
- A `thumbs` child job, fanned out next to `audio`, makes the poster, a sprite sheet (one 160 px tile every *N* seconds, with *N* chosen so a video has at most about 100 tiles) and `storyboard.vtt` with `#xywh=` cues.
- Drop the API's on-demand ffmpeg run. The poster is still made from the source first, so it shows right after upload.
- The dashboard player shows hover previews on the timeline.
**Done when:** hovering anywhere on a 1-hour video shows the right frame within a tile.

### 8. Retry a failed video
`POST /videos/{id}/retry`: in one transaction, reset `attempts` on the failed root and its failed children and set them back to `queued`. Fences are never reset. Add a "Retry" button in the dashboard.

### 9. Dashboard polish
A webhook "Redeliver" button, errors grouped by reason, and a "copy curl" for each video.

## Phase 2: Scale and captions

### 10. Object storage (S3 / MinIO)
Workers on many machines need shared storage. Add an S3 implementation of `storage.Storage`, with pre-signed range reads so a chunk worker fetches only its own byte range. Add MinIO to `docker-compose.yml` as an optional profile. Move this into Phase 1 if the first user needs more than one machine.

### 11. Workers on many machines, including GPUs
- Add a `requires TEXT[]` column on jobs. Workers set `CAPABILITIES=gpu,nvenc`, and `Claim` adds `AND requires <@ $caps`. CPU workers never take GPU-only jobs; GPU workers may take anything.
- Add an NVENC encoder profile (`h264_nvenc`, B-frames off, pinned profile and level) and teach validation to accept its output. Benchmark it against libx264 per rung.
- Show worker liveness in the dashboard: worker ids are already in `lease_owner`, so add a heartbeat table.

### 12. Claim benchmark
This does not exist yet; the old `cmd/bench` measured editor seeking. Write `cmd/bench-claim`: spin up *N* simulated workers (10 → 500) with sleep jobs against one Postgres, and measure claims per second, p50 and p99 claim latency, reaper cost and lock waits. Find where `SKIP LOCKED` contention or the claim index stops scaling.
**Output:** the README line "Good for up to X videos a day on one machine, Y workers per Postgres", and the numbers behind a flat-cost comparison for course platforms.

### 13. Auto captions with Whisper
- A separate `captions` root job, not a child, so `ready` never waits on captions. It shows as `captions: pending | ready | failed` on the video.
- Use whisper.cpp (CPU, `base` model) or faster-whisper on GPU workers (`requires = {gpu}`). Output WebVTT with language detection.
- Add an HLS `SUBTITLES` group in the master playlist and a `<track>` in the embed player.
**Done when:** a 10-minute talk gets captions you can read, and killing the captions worker mid-run still ends in captions.

This is also where the AI-products story starts: transcripts make videos searchable.

### 14. Metrics
`/metrics` for Prometheus: queue depth by kind, claim latency, encode seconds per minute of video, failures by reason, webhook backlog.

## Phase 3: Production readiness

- Hashed API keys with scopes, several keys, and a dashboard login (the single `API_KEY` from item 4 comes first).
- Signed playback URLs (HMAC with an expiry) for private video, and a CORS setting for players on other domains.
- Upload limits and rate limiting per key.
- Cache rendered playlists for high request rates.
- Storage lifecycle: optionally delete sources after *N* days, and recover orphaned files.

## Why the code alone is not enough

Mux, Cloudflare Stream, Bunny Stream and api.video already sell "upload a video, get HLS back", and an AI tool can write a basic ffmpeg-to-HLS pipeline in a weekend. So the code is not the moat. What is scarce: users who depend on it, reliability under real load (the chaos suite), and knowing which problem hurts enough that people pay. YC funds traction and insight, not repos.

## Proof that it is a real product

1. **One real user** besides you, such as a friend's course site. One real user beats 100 features.
2. The "Why we built this" story and the engineering post ("how I made distributed transcoding crash-safe").
3. Honest limits in the README, with numbers from the claim benchmark.
4. Then apply to YC with whatever traction and learning there is.

## Decisions

| Decision | Choice |
|---|---|
| First customer | **Decided 6 Nov 2026:** course and edtech platforms, pitched on ownership and a flat cost |
| Order of Version 1 | **Decided 6 Nov 2026:** HLS everywhere → ladder → webhooks → API key → public demo. tus, storyboard, retry and polish wait for users |
| Ladder: one job per chunk for all rungs, or one per chunk per rung | One job per chunk (decode once). Revisit once GPU workers exist |
| tus: library or hand-rolled | tusd's handler, when it is built |
| Name | Open. "Reelay" is a working name. Renaming means changing the module path and replacing `reelay` across the repo |
| License | Open. MIT for the most adoption, or AGPL-3.0 to stop a closed hosted fork. Decide before the first public post |
