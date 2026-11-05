# The Conduit engine

Reelay runs on Conduit, a crash-safe video pipeline. It turns an uploaded video into a low-resolution proxy that a browser editor can scrub smoothly. Long videos are split into chunks that encode in parallel and can be played before the whole job finishes. Correctness under failure comes from leases, fencing tokens and output validation, and a failure-injection suite checks it.

Go, Postgres (pgx), ffmpeg/ffprobe through `os/exec`, and local disk storage.

> This page describes the engine as it was first built: one 540p output, tuned for scrubbing in an editor. Reelay's `/videos` API sits on top of it unchanged. The quality ladder (360p/720p/1080p) and other product changes are planned in [PLAN.md](../PLAN.md).

## Failure tests

Every row is produced by `go run ./cmd/chaos`. The suite runs real worker processes against real Postgres and ffmpeg, injects each failure, and checks the outcome. The results below are from a real run.

| # | Failure injected | Expected outcome | Result |
|---|---|---|---|
| 1 | Kill worker mid-transcode (`kill -9`) | Lease expires, another worker reclaims, job ends done; no orphaned ffmpeg | ✅ A killed 2s into the transcode; B reaped the lease and finished: done at fence 2, no orphaned ffmpeg |
| 2 | Freeze worker past its lease (`SIGSTOP`), then resume | Its late commit is rejected by the fence | ✅ A froze holding fence 1 for 6.6s; B reclaimed and committed fence 2; on resume A's commit matched 0 rows and its proxy was deleted |
| 3 | Worker produces a truncated file | Validation catches it; job retried, never done with a bad file | ✅ attempt 1 rejected (video lasts 2.433s, source 4.000s); attempt 2 done, committed proxy re-validated |
| 4 | Same job dispatched twice | Only one result committed | ✅ 2 ffmpeg ran for the job at once; A's next heartbeat failed and it stopped; exactly one proxy committed (fence 2) |
| 5 | Same upload submitted 5× at once (client retries) | One job, one source file | ✅ 1×201 + 4×200, all naming the same job; 1 source file kept; processed once to done |
| 6 | Database restart mid-run | Jobs resume from stored state and finish | ✅ Postgres restarted mid-transcode (500ms); workers logged 2 DB errors and kept going; all 3 jobs done |
| 7 | Job fails every attempt | Ends failed after max retries, with the reason | ✅ 3 attempts, each rejected by validation, claims 2.1s apart (jittered backoff); ended failed with the validation error |
| 8 | Chunked: kill worker mid-chunk (`kill -9`) | Only that chunk is redone; other chunks keep their first result; seams valid, job done | ✅ A killed mid-chunk 1; only that chunk was redone (fence 2), the other 10 children kept their first result; seams re-validated, root done |
| 9 | Chunked: freeze a chunk worker past its lease, then resume | Its late commit is rejected and the fan-in counts the chunk once | ✅ A froze before committing audio (fence 1); B redid it and the rest; on resume A's commit matched 0 rows, so the root finalized exactly once (fence 2); stale output deleted |
| 10 | Chunked: one child fails every attempt | Root and every unfinished sibling end failed, citing the child | ✅ audio failed 3/3 attempts; in the same transaction the root failed ("audio failed: attempt 3 of 3: invalid proxy: …") and 10 unfinished siblings were cancelled |
| 11 | Chunked: database restart during fan-out | Committed chunks are kept; the rest finish; job done | ✅ Postgres restarted with 1/11 children committed (600ms outage); the rest finished, root finalized and seams re-validated |
| 12 | Chunked: kill worker while planning chunks | No children are left behind; the next owner plans exactly once | ✅ A was killed after splitting the source but before writing the plan: 0 children existed; B reclaimed the root and planned once (10 chunks + audio), root done at fence 3 |
| 13 | Chunked: kill worker while checking the seams | Only the final check reruns; no chunk is redone | ✅ A was killed mid-finalize (root validating, fence 2); its lease expired and B reran only the finalize step: all 11 children kept fence 1, root done at fence 3 |
| 14 | Invariants across all jobs above | Nothing left active; every done job's output exists and passes validation; no stray files | ✅ 81 jobs (chunks included): 68 done (every output re-validated), 13 failed; 0 active, 0 stray proxies, 0 leftover work dirs |

The same scenarios can be run and watched in a browser with `./reelay chaos`, a local console that shows the job table, live worker logs, and a player for chunked jobs.

## How a job stays correct

```
queued ──claim──▶ running ──▶ validating ──commit──▶ done
   ▲                 │              │
   └──── retry ──────┴──────────────┴──▶ failed   (permanent error or out of attempts)
         (backoff with jitter; lease expiry counts as an attempt)
```

- **Claiming.** `UPDATE … WHERE id = (SELECT … FOR UPDATE SKIP LOCKED)` gives each job to exactly one worker. Concurrent claimers skip rows that are locked instead of waiting for them.
- **Leases.** A claim lasts a fixed time measured on the database clock, and the worker renews it with heartbeats. A reaper (running inside every worker) returns jobs whose lease ran out to the queue. If a worker loses its lease, ffmpeg is cancelled.
- **Fencing.** Every claim increments `fence`. Every write the worker makes (heartbeat, progress, validating, commit, fail) includes `WHERE fence = $n`, so a worker that paused past its lease changes nothing when it wakes up. Output paths include the fence as well (`proxies/<job>/<fence>.mp4`), so a stale worker and the current owner never write the same file. *The lease decides who should be working on a job; the fence decides whose write counts.*
- **Validation before done.** A proxy is checked in storage, not in the worker's temp directory, before it can commit. A file that fails validation is retried and never marked done.
- **The database enforces the invariants.** `CHECK` constraints in [schema.sql](../internal/db/schema.sql) reject impossible rows even if the Go code is wrong. For example, a `done` job must have an output, and a running job must have a lease.
- **Duplicate uploads.** An `Idempotency-Key` header maps retried uploads to a single job and a single stored source.
- **A worker killed outright.** On Linux, ffmpeg runs with `Pdeathsig`, so the kernel kills it along with its worker. Scratch directories left behind by earlier attempts are removed by the next worker to claim that job, since a lower fence means the earlier owner has lost the job.

## Chunked proxies

Sources at least `CHUNK_MIN_DURATION` long are split into chunks of `CHUNK_SECONDS` (default 10 s). The default is 0, so every source is chunked: one shorter than a chunk becomes a single chunk, and every video plays as HLS. Set it higher to send shorter sources down the single-file path described above; the chaos suite does this (2 minutes) so its single-file scenarios keep running.

```
upload's job (root)
  │ plan: probe every frame's timestamp, split into ranges; in ONE transaction fenced
  │ on the root, insert the children and park the root in 'waiting' (pending_deps = N+1)
  ├── audio    (claimed first)
  ├── chunk 0  ┐
  ├── …        ├ ordinary jobs: claim, lease, heartbeat, fence, retry, reaper
  └── chunk N-1┘
  │ each child's fenced commit decrements the root's pending_deps in the SAME transaction;
  │ the commit that reaches 0 re-queues the root
  ▼
root again: finalize — validate the committed chunks as one proxy, then commit done
```

- **Fan-in happens exactly once.** A child can commit only once, because its commit is guarded by the fence and `state = 'validating'`. The decrement happens inside that same commit, so each child counts once no matter how many times it was retried. The row lock on the root makes concurrent decrements run one after another, so exactly one commit takes the count to zero. Tested by committing 13 children concurrently, 30 times, under the race detector.
- **Planning is atomic.** If the planner has lost the root, its transaction matches zero rows and rolls back, so no duplicate children are created. A crash during planning leaves nothing behind.
- **Failure spreads.** When a child runs out of attempts, the same transaction fails the root and every unfinished sibling. A sibling that is mid-transcode finds out on its next heartbeat, and its ffmpeg is killed.
- **No waiting parent.** The root holds no lease while its children run, so a busy root can never occupy a worker that its own children need.
- **Every frame lands in exactly one chunk.** Chunk ranges are half-open `[start, end)` on the source's own timestamps. Boundaries sit halfway between two frames, so rounding can never place a frame in two chunks or in none. Chunk lengths are multiples of the keyframe interval, so keyframes stay evenly spaced across seams.
- **Audio is never chunked.** Every AAC encode starts with a short silent lead-in, so joined chunks would click or drift at each seam. If the source's audio is already AAC-LC (as from phones and most cameras), it is copied, which takes 0.1 s for a 2-minute clip instead of 6.5 s to encode. That matters because playback cannot start until audio is in.

### Playback while encoding

The API serves HLS:

| Endpoint | Contents |
|---|---|
| `/jobs/{id}/hls/master.m3u8` | Master playlist |
| `/jobs/{id}/hls/video.m3u8` | `EVENT` playlist that grows as chunks commit |
| `/jobs/{id}/hls/audio.m3u8` | Audio playlist |
| `/jobs/{id}/hls/…` | Segments, with immutable caching (every path carries its fence) |

- **Playlists are built from the job table on every request, never stored.** Concurrent commits can't race to write a playlist file, and a segment is listed only after its fenced commit.
- **The video playlist lists the committed chunks from chunk 0 up to the first gap.** This run only ever grows, which is what an `EVENT` playlist allows.
- **Playback starts** once audio and chunk 0 have committed.
- **Limit:** at high request rates the playlist rendering is the part to cache.

**A bug found by testing in a real browser.** Every check passed, but chunks after the first played on top of chunk 0. The muxer had started each chunk's media at 0 and stored the real offset in an MP4 edit list. ffprobe applies edit lists; browsers' Media Source Extensions ignore them and place each fragment by its `tfdt`. The fix is `movflags=+frag_discont`. Validation now reads the fMP4 boxes directly ([mp4box.go](../internal/media/mp4box.go)) and rejects any chunk whose `tfdt` disagrees with its frames. Without the fix, the chunk tests fail with this check.

## Proxy settings

Every setting serves one goal: smooth scrubbing in a browser.

| Setting | Choice | Why |
|---|---|---|
| Codec / container | H.264 (yuv420p) in MP4, or fMP4 HLS for chunked jobs | Decodes in every browser |
| Resolution | 540p, never upscaled | Small enough to decode quickly while scrubbing |
| Keyframes | Every 1 s, fixed (`-force_key_frames`, `-sc_threshold 0`) | A seek never decodes more than 1 s; chunk boundaries line up with keyframes |
| `+faststart` (single file) | Index (moov atom) at the front | Playback starts before the whole file downloads |
| B-frames (chunks) | Off; H.264 High profile at level 4.0 | Decode order equals display order, so no seam depends on the previous chunk; the profile and level match the `CODECS` value in the master playlist |
| Audio | AAC; copied when the source is already AAC-LC | Editors need audio while scrubbing; copying makes it available almost at once |

**Validation** (ffprobe plus the box reader). All proxies are checked for:
- codec, pixel format and height
- duration measured from packets, not the header, because a truncated `faststart` file still declares its full duration
- starts on a keyframe, and no keyframe gap exceeds the interval plus one frame
- audio length within 100 ms of the source

**Chunks are also checked for:**
- exactly the planned frame count, with every frame inside its range
- timestamps that strictly increase
- a browser timeline that agrees with the frames (`tfdt` check)

**The assembled proxy is also checked for:**
- total frames equal to the source's frame count
- no gap or overlap at any seam
- even keyframe spacing across seams

## Benchmark

Test input: a 120 s 1080p30 source with AAC audio. Machine: 16 cores, one machine, local disk. Each configuration was run twice; the runs agreed within 0.5 s.

| Path | Workers | Playable after | Done after |
|---|---|---|---|
| Single file | 1 | 12.5 s (only when done) | 12.5 s |
| Chunked, 10 s chunks | 1 | 1.5 s | 10.5 s |
| Chunked | 2 | 1.1 s | 7.5 s |
| Chunked | 4 | 2.1 s | 7.1 s |
| Chunked | 8 | 4.0 s | 7.3 s |

What the numbers show:
- **Time until the editor can start drops from 12.5 s to 1–2 s.** This is the main benefit.
- **Total time improves less: from 12.5 s to about 7 s.** x264 already uses every core, so extra workers on one machine compete for the same CPU. Chunked with 1 worker beats the single file mainly because its audio is copied rather than encoded, not because of parallelism.
- **More workers on one machine delay the first playable moment.** With 8 workers, chunk 0 competes with 7 other chunks for CPU. The gains from chunking across machines are not measured here.
- **Retries are cheap.** A failed chunk costs one chunk to redo (about 0.4–0.7 s here) instead of the whole file (12.3 s).

## Running it

```bash
docker compose up -d postgres        # Postgres on :5435
go build -o reelay .
./reelay serve                       # API and dashboard on :8080
./reelay worker                      # run as many as you like

curl -F file=@video.mp4 localhost:8080/jobs            # → job id
curl localhost:8080/jobs/<id>                          # state, fence, attempts, progress
curl localhost:8080/jobs/<id>/children                 # chunk and audio jobs, if chunked
# chunked jobs: play localhost:8080/jobs/<id>/hls/master.m3u8 (hls.js, Safari)

go test ./...                        # needs Postgres and ffmpeg
go run ./cmd/chaos                   # the failure table above
./reelay chaos                       # browser console on 127.0.0.1:8090
```

Worker settings (environment variables):

| Variable | Default | Meaning |
|---|---|---|
| `LEASE_TTL` | 30s | How long a claim lasts without a heartbeat |
| `REAP_INTERVAL` | 5s | How often the reaper checks for expired leases |
| `POLL_INTERVAL` | 1s | How often an idle worker looks for jobs |
| `CHUNK_SECONDS` | 10s | Chunk length (a multiple of the 1 s keyframe interval) |
| `CHUNK_MIN_DURATION` | 0 | Sources at least this long are chunked; 0 chunks every source |

## Code map

| Path | What |
|---|---|
| [internal/jobs/store.go](../internal/jobs/store.go) | Job states, claim, heartbeat, fenced commit and fail, reaper |
| [internal/jobs/graph.go](../internal/jobs/graph.go) | Chunked job graph: plan, fan-in commit, failure propagation |
| [internal/db/schema.sql](../internal/db/schema.sql) | Table and `CHECK` constraints that encode the invariants |
| [internal/worker/worker.go](../internal/worker/worker.go) | Claim loop, heartbeats, commit/discard of each attempt |
| [internal/worker/chunked.go](../internal/worker/chunked.go) | Plan, chunk, audio and finalize steps |
| [internal/media/](../internal/media/) | ffmpeg/ffprobe: transcode, chunk planning, validation, fMP4 box reader |
| [internal/hls/hls.go](../internal/hls/hls.go) | Segment types and playlist rendering (pure, no I/O) |
| [internal/api/](../internal/api/) | Upload, job status, HLS playback |
| [cmd/chaos/](../cmd/chaos/) | Failure-injection suite |
| [internal/chaos/](../internal/chaos/) | Browser chaos console |

## Not done yet

- **Object storage.** Storage is local disk behind a small interface. With S3/MinIO, each chunk worker should read only its own byte range through a pre-signed URL; today every worker reads the whole source.
- **Playlist caching**, if playlists are requested at high rates.
- **Metrics.** Logs are structured, but nothing is exported.
- **A workflow engine.** The job graph is deliberately a Postgres queue with a dependency counter. Branching graphs, steps that wait on people, or fan-out across services would justify moving to something like Temporal.
