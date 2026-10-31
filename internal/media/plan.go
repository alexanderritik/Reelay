package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SourceInfo is what the worker needs to choose between the single-file and
// chunked paths, and to plan the chunked one.
type SourceInfo struct {
	Duration time.Duration
	HasAudio bool
	Height   int
}

// Probe reads a source's stream metadata. Errors wrap ErrBadSource when the
// file itself is at fault.
func Probe(ctx context.Context, path string) (SourceInfo, error) {
	s, err := probeSource(ctx, path)
	if err != nil {
		return SourceInfo{}, err
	}
	return SourceInfo{Duration: time.Duration(s.videoDur * float64(time.Second)), HasAudio: s.hasAudio, Height: s.height}, nil
}

// Chunk is a half-open range [Start, End) of source presentation time, in
// seconds, and the number of source video frames inside it.
type Chunk struct {
	Start, End float64
	Frames     int
}

// SourceFrames returns the presentation time of every video frame in the
// source, sorted. It reads packet headers only (no decoding), so it costs
// one pass over the file. Errors wrap ErrBadSource when the file is at fault.
func SourceFrames(ctx context.Context, path string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", path)
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffprobe: %w", err)
	}
	var pts []float64
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSuffix(strings.TrimSpace(sc.Text()), ",")
		if t, err := strconv.ParseFloat(line, 64); err == nil && t >= 0 { // skip N/A and pre-roll
			pts = append(pts, t)
		}
	}
	if err := cmd.Wait(); err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: ffprobe: %w: %s", ErrBadSource, err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	if len(pts) == 0 {
		return nil, fmt.Errorf("%w: no video frames", ErrBadSource)
	}
	slices.Sort(pts)
	return pts, nil
}

// PlanChunks splits a source's frames into chunks about chunkLen seconds
// long, on a grid anchored at the first frame. chunkLen should be a multiple
// of the keyframe interval, so every chunk starts on the proxy's keyframe
// grid and the assembled proxy keeps even keyframe spacing across seams.
//
// Boundaries sit halfway between two frames, never on one, so the frame
// counts here (from ffprobe's rounded timestamps) always agree with what
// ffmpeg's trim filter keeps (from exact ones). Every frame lands in exactly
// one chunk: the counts sum to len(frames).
func PlanChunks(frames []float64, chunkLen float64) []Chunk {
	if len(frames) == 0 || chunkLen <= 0 {
		return nil
	}
	gap := 0.01 // half-frame margin for the outer edges
	if len(frames) > 1 {
		gap = frames[1] - frames[0]
	}

	start := math.Max(0, frames[0]-gap/2)
	var chunks []Chunk
	i := 0 // first frame of the current chunk
	for k := 1; i < len(frames); k++ {
		grid := frames[0] + float64(k)*chunkLen
		j := i
		for j < len(frames) && frames[j] < grid {
			j++
		}
		if j == i {
			continue // no frames in this grid step (a gap in the source)
		}
		end := frames[len(frames)-1] + 1 // past the last frame
		if j < len(frames) {
			end = (frames[j-1] + frames[j]) / 2
		}
		chunks = append(chunks, Chunk{Start: start, End: end, Frames: j - i})
		start, i = end, j
	}
	return chunks
}
