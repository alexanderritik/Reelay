package worker

// The chunked path for long sources. An upload's job plans and fans out
// (processUpload → plan), its chunk and audio jobs each encode and commit one
// HLS rendition (processChunk, processAudio), and when the last one commits
// the root comes back to check the whole (finalize). The job graph rules
// live in jobs/graph.go; this file only does the work for each step.
//
// Storage layout, with the fence in every path so a stale worker and the
// current owner never write the same files:
//
//	proxies/<root>/v/<chunk>/<fence>/index.m3u8, init.mp4, seg_NNN.m4s
//	proxies/<root>/a/<fence>/index.m3u8, init.mp4, seg_NNN.m4s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexanderritik/reelay/internal/hls"
	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
	"github.com/alexanderritik/reelay/internal/storage"
)

// processUpload picks the path for an upload's job: sources shorter than
// ChunkMinDuration take the single-file path, the rest are planned into
// chunks. With ChunkMinDuration 0 (the product default) every source is
// chunked; one shorter than ChunkSeconds becomes a single chunk.
func (w *Worker) processUpload(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	dir, err := w.workDir(job)
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)

	src, err := storage.LocalFile(ctx, w.Storage, job.SourceKey, dir)
	if err != nil {
		return result{}, fmt.Errorf("fetch source %s: %w", job.SourceKey, err)
	}
	info, err := media.Probe(ctx, src)
	if err != nil {
		return result{}, err
	}
	if w.ChunkSeconds <= 0 || info.Duration < w.ChunkMinDuration {
		return w.processSingle(ctx, log, job)
	}
	return w.plan(ctx, log, job, src, info)
}

// plan splits the source into chunks and fans the job out. Plan is the
// fenced write, so there is nothing left to commit afterwards.
func (w *Worker) plan(ctx context.Context, log *slog.Logger, job jobs.Job, src string, info media.SourceInfo) (result, error) {
	frames, err := media.SourceFrames(ctx, src)
	if err != nil {
		return result{}, err
	}
	planned := media.PlanChunks(frames, w.ChunkSeconds.Seconds())
	specs := make([]jobs.ChunkSpec, len(planned))
	for i, c := range planned {
		specs[i] = jobs.ChunkSpec{Start: c.Start, End: c.End, Frames: c.Frames}
	}
	w.chaosCrash(log, ChaosCrashDuringPlan)
	if err := w.Jobs.Plan(ctx, job.ID, job.Fence, specs, info.HasAudio); err != nil {
		return result{}, err
	}
	log.Info("planned chunks", "chunks", len(specs), "audio", info.HasAudio,
		"frames", len(frames), "duration", info.Duration.Round(time.Millisecond))
	return result{output: fmt.Sprintf("%d chunks", len(specs))}, nil
}

// processChunk encodes, stores and validates one chunk. Its commit also
// counts it off the root (jobs.CommitChild).
func (w *Worker) processChunk(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	start, end, frames := *job.RangeStart, *job.RangeEnd, *job.ExpectedFrames
	prefix := fmt.Sprintf("proxies/%s/v/%d/%d", *job.ParentID, *job.ChunkIndex, job.Fence)
	log = log.With("chunk", *job.ChunkIndex)

	// Progress counts frames: the plan knows exactly how many to expect.
	// reportProgress takes durations, so frames are passed as nanoseconds.
	onProgress, stopProgress := w.reportProgress(ctx, log, job, time.Duration(frames))
	defer stopProgress()

	return w.rendition(ctx, log, job, prefix,
		func(src, out string) (hls.Media, error) {
			log.Info("transcoding chunk", "start", start, "end", end, "frames", frames)
			return media.TranscodeChunk(ctx, src, out, w.Proxy, start, end,
				func(n int) { onProgress(time.Duration(n)) })
		},
		func(src, playlist string) error {
			return media.ValidateChunk(ctx, src, playlist, w.Proxy, start, end, frames)
		})
}

// processAudio encodes, stores and validates the whole audio track.
func (w *Worker) processAudio(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	prefix := fmt.Sprintf("proxies/%s/a/%d", *job.ParentID, job.Fence)
	return w.rendition(ctx, log, job, prefix,
		func(src, out string) (hls.Media, error) {
			log.Info("transcoding audio")
			return media.TranscodeAudio(ctx, src, out)
		},
		func(src, playlist string) error {
			return media.ValidateAudio(ctx, src, playlist)
		})
}

// rendition is the shared shape of a chunk or audio attempt: encode into the
// work dir, store every file under prefix, move to validating, validate what
// is actually stored, and hand back a fenced CommitChild.
func (w *Worker) rendition(ctx context.Context, log *slog.Logger, job jobs.Job, prefix string,
	encode func(src, out string) (hls.Media, error), validate func(src, playlist string) error,
) (result, error) {
	dir, err := w.workDir(job)
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)

	src, err := storage.LocalFile(ctx, w.Storage, job.SourceKey, dir)
	if err != nil {
		return result{}, fmt.Errorf("fetch source %s: %w", job.SourceKey, err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		return result{}, err
	}
	began := time.Now()
	m, err := encode(src, out)
	if err != nil {
		return result{}, err
	}

	res := result{
		output:  prefix,
		discard: func(ctx context.Context) error { return w.Storage.DeletePrefix(ctx, prefix) },
	}
	var bytes int64
	for _, f := range media.Files(m) {
		n, err := putFile(ctx, w.Storage, path.Join(prefix, f), filepath.Join(out, f))
		if err != nil {
			return res, fmt.Errorf("store %s: %w", f, err)
		}
		bytes += n
	}
	log.Info("rendition written", "prefix", prefix, "segments", len(m.Segments), "bytes", bytes,
		"took", time.Since(began).Round(time.Millisecond))
	if err := w.chaosAfterStore(ctx, log, path.Join(prefix, m.Segments[0].URI), dir); err != nil {
		return res, err
	}

	if err := w.Jobs.MarkValidating(ctx, job.ID, job.Fence); err != nil {
		return res, err
	}
	playlist, err := fetchRendition(ctx, w.Storage, prefix, m, filepath.Join(dir, "stored"))
	if err != nil {
		return res, fmt.Errorf("fetch stored rendition: %w", err)
	}
	if err := validate(src, playlist); err != nil {
		return res, err
	}
	log.Info("rendition valid", "prefix", prefix)
	res.commit = func(ctx context.Context) error { return w.Jobs.CommitChild(ctx, job.ID, job.Fence, prefix, m) }
	return res, nil
}

// finalize runs on the root once every child has committed: it validates the
// committed chunks as one proxy (frame total, seams, keyframes across seams)
// and marks the root done. A seam problem cannot be fixed by re-checking the
// same chunks, so it fails the job for good rather than retrying.
func (w *Worker) finalize(ctx context.Context, log *slog.Logger, job jobs.Job) (result, error) {
	dir, err := w.workDir(job)
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)

	kids, err := w.Jobs.Children(ctx, job.ID)
	if err != nil {
		return result{}, err
	}
	var playlists []string
	for _, k := range kids {
		if k.State != jobs.StateDone || k.OutputKey == nil || k.HLS == nil {
			return result{}, fmt.Errorf("child %s is %s, want done", k.ID, k.State)
		}
		if k.Kind != jobs.KindChunk {
			continue // audio was checked against the source before it committed
		}
		pl, err := fetchRendition(ctx, w.Storage, *k.OutputKey, *k.HLS,
			filepath.Join(dir, fmt.Sprint(*k.ChunkIndex)))
		if err != nil {
			return result{}, fmt.Errorf("fetch chunk %d: %w", *k.ChunkIndex, err)
		}
		playlists = append(playlists, pl)
	}

	src, err := storage.LocalFile(ctx, w.Storage, job.SourceKey, dir)
	if err != nil {
		return result{}, fmt.Errorf("fetch source %s: %w", job.SourceKey, err)
	}
	frames, err := media.SourceFrames(ctx, src)
	if err != nil {
		return result{}, err
	}

	if err := w.Jobs.MarkValidating(ctx, job.ID, job.Fence); err != nil {
		return result{}, err
	}
	w.chaosCrash(log, ChaosCrashDuringFinalize)
	log.Info("validating assembled proxy", "chunks", len(playlists))
	if err := media.ValidateAssembly(ctx, src, playlists, w.Proxy, len(frames)); err != nil {
		if _, ok := errors.AsType[*media.InvalidError](err); ok {
			return result{}, fmt.Errorf("%w: %w", errPermanent, err)
		}
		return result{}, err
	}

	key := "proxies/" + job.ID // the API serves the playlists under this prefix
	return result{
		output: key,
		commit: func(ctx context.Context) error { return w.Jobs.Commit(ctx, job.ID, job.Fence, key) },
	}, nil
}

// workDir creates scratch space for one attempt. It first removes scratch
// left by earlier attempts at this job: their fence is lower, so their owners
// have lost the job by definition, and a worker killed outright never got to
// clean up after itself. (On a shared disk only; another machine's leftovers
// are swept when it next claims that job.)
func (w *Worker) workDir(job jobs.Job) (string, error) {
	stale, _ := filepath.Glob(filepath.Join(w.WorkDir, fmt.Sprintf("job-%s-f*-*", job.ID)))
	for _, d := range stale {
		var fence int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(filepath.Base(d), "job-"+job.ID+"-f"), "%d-", &fence); err == nil && fence < job.Fence {
			_ = os.RemoveAll(d)
		}
	}
	dir, err := os.MkdirTemp(w.WorkDir, fmt.Sprintf("job-%s-f%d-", job.ID, job.Fence))
	if err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	return dir, nil
}

// fetchRendition makes a stored rendition readable by ffprobe and returns
// its playlist's local path. Local storage is read in place; other storage
// is downloaded into dir (segment names are flat, so they stay relative).
func fetchRendition(ctx context.Context, s storage.Storage, prefix string, m hls.Media, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var playlist string
	for _, f := range media.Files(m) {
		p, err := storage.LocalFile(ctx, s, path.Join(prefix, f), dir)
		if err != nil {
			return "", err
		}
		if f == media.PlaylistName {
			playlist = p
		}
	}
	return playlist, nil
}
