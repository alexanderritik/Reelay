package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexanderritik/reelay/internal/hls"
)

// Checks for chunked (HLS) proxies. Each chunk is checked alone before it
// commits; once every chunk is in, ValidateAssembly checks the seams between
// them. Like Validate, an *InvalidError means the output is bad; any other
// error means the check itself could not run.

const tsEps = 0.001 // timestamp rounding in ffprobe's 6-decimal output

// track is one rendition's video stream and its packets in decode order.
type track struct {
	stream *stream
	frame  float64 // nominal frame duration, seconds
	pkts   []videoPacket
}

type videoPacket struct {
	pts, dur float64
	key      bool
}

func (t track) first() float64 { return t.pkts[0].pts }
func (t track) end() float64 {
	last := t.pkts[len(t.pkts)-1]
	return last.pts + last.dur
}

// probeTrack reads the video track of an HLS playlist (or any file), and
// returns the whole probe too. A file that cannot be read is invalid.
func probeTrack(ctx context.Context, path string) (track, proxyInfo, *InvalidError) {
	p, err := probeProxy(ctx, path)
	if err != nil {
		return track{}, p, &InvalidError{[]string{fmt.Sprintf("cannot read %s: %v", path, err)}}
	}
	if p.video == nil {
		return track{}, p, &InvalidError{[]string{"no video stream"}}
	}
	t := track{stream: p.video, frame: 1 / parseRate(p.video.AvgFrameRate)}
	for _, pk := range p.packets {
		if pk.StreamIndex != p.video.Index || pk.PtsTime == "" || pk.PtsTime == "N/A" {
			continue
		}
		t.pkts = append(t.pkts, videoPacket{
			pts: parseFloat(pk.PtsTime), dur: parseFloat(pk.DurationTime), key: strings.Contains(pk.Flags, "K"),
		})
	}
	if len(t.pkts) == 0 {
		return track{}, p, &InvalidError{[]string{"no video frames"}}
	}
	if math.IsInf(t.frame, 0) || t.frame <= 0 {
		t.frame = t.pkts[0].dur
	}
	return t, p, nil
}

// ValidateChunk checks one chunk's stored rendition against its plan:
//   - h264 yuv420p at the proxy height, video only
//   - exactly the planned number of frames, every one inside [start, end)
//   - timestamps strictly increase in decode order (no B-frames, no
//     duplicates), so the chunk is exactly the planned run of source frames
//   - it starts on a keyframe and no keyframe gap exceeds the interval + 1 frame
func ValidateChunk(ctx context.Context, src, playlist string, spec ProxySpec, start, end float64, frames int) error {
	s, err := probeSource(ctx, src)
	if err != nil {
		return fmt.Errorf("probe source: %w", err)
	}
	t, p, invalid := probeTrack(ctx, playlist)
	if invalid != nil {
		return invalid
	}

	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	checkFormat(bad, t.stream, min(spec.Height, s.height))
	if p.audio != nil {
		bad("chunk has an audio stream; audio is encoded separately")
	}

	if len(t.pkts) != frames {
		bad("%d frames, planned %d (frames lost or duplicated)", len(t.pkts), frames)
	}
	if t.first() < start-tsEps || t.pkts[len(t.pkts)-1].pts >= end+tsEps {
		bad("frames span %.3fs-%.3fs, outside planned range [%.3f, %.3f)",
			t.first(), t.pkts[len(t.pkts)-1].pts, start, end)
	}
	for i := 1; i < len(t.pkts); i++ {
		if t.pkts[i].pts <= t.pkts[i-1].pts {
			bad("timestamps not increasing at frame %d (%.3fs after %.3fs)", i, t.pkts[i].pts, t.pkts[i-1].pts)
			break
		}
	}
	if !t.pkts[0].key {
		bad("chunk does not start on a keyframe")
	}
	checkKeyframeGaps(bad, t, spec)
	checkBrowserTimeline(bad, playlist, t)

	if len(problems) > 0 {
		return &InvalidError{problems}
	}
	return nil
}

// ValidateAudio checks the stored audio rendition: AAC, as long as the
// source's audio within audioTolerance.
func ValidateAudio(ctx context.Context, src, playlist string) error {
	s, err := probeSource(ctx, src)
	if err != nil {
		return fmt.Errorf("probe source: %w", err)
	}
	p, err := probeProxy(ctx, playlist)
	if err != nil {
		return &InvalidError{[]string{fmt.Sprintf("cannot read audio: %v", err)}}
	}
	if p.audio == nil {
		return &InvalidError{[]string{"no audio stream"}}
	}
	var problems []string
	if p.audio.CodecName != "aac" {
		problems = append(problems, fmt.Sprintf("audio codec %s, want aac", p.audio.CodecName))
	}
	aStart, aEnd := p.span(p.audio.Index)
	if d := aEnd - aStart; math.Abs(d-s.audioDur) > audioTolerance.Seconds() {
		problems = append(problems, fmt.Sprintf("audio lasts %.3fs, source %.3fs", d, s.audioDur))
	}
	if len(problems) > 0 {
		return &InvalidError{problems}
	}
	return nil
}

// ValidateAssembly checks the committed chunks, in order, as one proxy:
//   - their frames add up to the source's (sourceFrames)
//   - each chunk starts exactly where the previous one ended: no gap and no
//     overlap at any seam
//   - keyframe spacing stays within the interval + 1 frame across seams
//   - together they last as long as the source video, within one frame
func ValidateAssembly(ctx context.Context, src string, chunkPlaylists []string, spec ProxySpec, sourceFrames int) error {
	s, err := probeSource(ctx, src)
	if err != nil {
		return fmt.Errorf("probe source: %w", err)
	}

	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	var tracks []track
	total := 0
	for i, pl := range chunkPlaylists {
		t, _, invalid := probeTrack(ctx, pl)
		if invalid != nil {
			bad("chunk %d: %v", i, invalid)
			continue
		}
		checkBrowserTimeline(func(format string, args ...any) {
			bad("chunk %d: "+format, append([]any{i}, args...)...)
		}, pl, t)
		tracks = append(tracks, t)
		total += len(t.pkts)
	}
	if len(problems) > 0 {
		return &InvalidError{problems}
	}

	if total != sourceFrames {
		bad("chunks hold %d frames, source has %d", total, sourceFrames)
	}
	maxGap := spec.KeyframeInterval.Seconds() + tracks[0].frame + tsEps
	for i := 1; i < len(tracks); i++ {
		prev, next := tracks[i-1], tracks[i]
		if d := next.first() - prev.end(); math.Abs(d) > tsEps {
			kind := "gap"
			if d < 0 {
				kind = "overlap"
			}
			bad("seam %d|%d: %s of %.3fs (chunk %d ends %.3fs, chunk %d starts %.3fs)",
				i-1, i, kind, math.Abs(d), i-1, prev.end(), i, next.first())
		}
		if g := next.first() - lastKey(prev); g > maxGap {
			bad("seam %d|%d: keyframe gap of %.3fs, max %.3fs", i-1, i, g, maxGap)
		}
	}
	span := tracks[len(tracks)-1].end() - tracks[0].first()
	if math.Abs(span-s.videoDur) > tracks[0].frame+tsEps {
		bad("proxy lasts %.3fs, source %.3fs", span, s.videoDur)
	}

	if len(problems) > 0 {
		return &InvalidError{problems}
	}
	return nil
}

// checkBrowserTimeline checks a rendition where a browser will place it, not
// where ffprobe says it is (see mp4box.go): no edit list may shift it, and
// every segment's tfdt must fall on the frames ffprobe reported, starting at
// the first one. Without this, a chunk can pass every other check and still
// play on top of chunk 0.
func checkBrowserTimeline(bad func(string, ...any), playlist string, t track) {
	b, err := os.ReadFile(playlist)
	if err != nil {
		bad("read playlist: %v", err)
		return
	}
	m, err := hls.Parse(string(b))
	if err != nil {
		bad("playlist: %v", err)
		return
	}
	dir := filepath.Dir(playlist)
	segs := make([]string, len(m.Segments))
	for i, s := range m.Segments {
		segs[i] = filepath.Join(dir, s.URI)
	}
	tl, err := readTimeline(filepath.Join(dir, m.Init), segs)
	if err != nil {
		bad("read fMP4 boxes: %v", err)
		return
	}
	if tl.shifted {
		bad("an edit list shifts the media in time; browsers ignore it, so the chunk would play at the wrong time")
	}
	if d := tl.starts[0] - t.first(); math.Abs(d) > tsEps {
		bad("first segment's tfdt is %.3fs but its first frame is at %.3fs: a browser would place it %.3fs off",
			tl.starts[0], t.first(), d)
	}
	for i := 1; i < len(tl.starts); i++ {
		if tl.starts[i] <= tl.starts[i-1] || tl.starts[i] >= t.end() {
			bad("segment %d tfdt %.3fs out of order or outside the chunk", i, tl.starts[i])
			return
		}
	}
}

func checkFormat(bad func(string, ...any), v *stream, height int) {
	if v.CodecName != "h264" {
		bad("video codec %s, want h264", v.CodecName)
	}
	if v.PixFmt != "yuv420p" {
		bad("pixel format %s, want yuv420p", v.PixFmt)
	}
	if v.Height != height {
		bad("height %d, want %d", v.Height, height)
	}
}

func checkKeyframeGaps(bad func(string, ...any), t track, spec ProxySpec) {
	maxGap := spec.KeyframeInterval.Seconds() + t.frame + tsEps
	prev := t.first()
	for _, pk := range t.pkts[1:] {
		if !pk.key {
			continue
		}
		if pk.pts-prev > maxGap {
			bad("keyframe gap of %.3fs at %.3fs, max %.3fs", pk.pts-prev, prev, maxGap)
			return
		}
		prev = pk.pts
	}
	if t.end()-prev > maxGap {
		bad("keyframe gap of %.3fs at %.3fs (to the end), max %.3fs", t.end()-prev, prev, maxGap)
	}
}

func lastKey(t track) float64 {
	for i := len(t.pkts) - 1; i >= 0; i-- {
		if t.pkts[i].key {
			return t.pkts[i].pts
		}
	}
	return t.first()
}
