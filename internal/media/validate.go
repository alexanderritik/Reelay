package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrBadSource means the source itself is unusable (unreadable, or no video).
// Retrying cannot help, so the job should fail permanently.
var ErrBadSource = errors.New("bad source")

// CheckSource confirms the source is a readable video before any work is
// spent on it, and returns its duration (zero if the file does not record
// one). Errors wrap ErrBadSource when the file itself is at fault.
func CheckSource(ctx context.Context, path string) (time.Duration, error) {
	info, err := probeSource(ctx, path)
	if err != nil {
		return 0, err
	}
	return time.Duration(info.videoDur * float64(time.Second)), nil
}

// audioTolerance is how far the proxy's audio length may differ from the
// source's. It covers AAC priming and frame padding (~20 ms per side), and
// is far below any drift a person would notice while scrubbing.
const audioTolerance = 100 * time.Millisecond

// InvalidError lists everything wrong with a proxy. It means the output is
// bad (retry the job); any other error from Validate means the check itself
// could not run.
type InvalidError struct {
	Problems []string
}

func (e *InvalidError) Error() string {
	return "invalid proxy: " + strings.Join(e.Problems, "; ")
}

// Validate checks the proxy at proxyPath against its source and spec:
//   - it opens and has a video stream
//   - h264, yuv420p, height min(spec.Height, source height)
//   - its packets span the source's duration within one frame (measured from
//     packets, not the header: a truncated faststart file still declares the
//     full duration up front)
//   - it starts on a keyframe and no keyframe gap exceeds the interval + 1 frame
//   - if the source has audio, it has AAC audio of the same length
func Validate(ctx context.Context, srcPath, proxyPath string, spec ProxySpec) error {
	src, err := probeSource(ctx, srcPath)
	if err != nil {
		return fmt.Errorf("probe source: %w", err)
	}

	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	p, err := probeProxy(ctx, proxyPath)
	if err != nil {
		bad("cannot read proxy: %v", err)
		return &InvalidError{problems}
	}
	if p.video == nil {
		bad("no video stream")
		return &InvalidError{problems}
	}

	v := p.video
	if v.CodecName != "h264" {
		bad("video codec %s, want h264", v.CodecName)
	}
	if v.PixFmt != "yuv420p" {
		bad("pixel format %s, want yuv420p", v.PixFmt)
	}
	if want := min(spec.Height, src.height); v.Height != want {
		bad("height %d, want %d", v.Height, want)
	}

	frame := 1 / parseRate(v.AvgFrameRate)
	if math.IsInf(frame, 0) || frame <= 0 {
		bad("unknown frame rate %q", v.AvgFrameRate)
		return &InvalidError{problems}
	}
	const eps = 0.001 // timestamp rounding

	vStart, vEnd := p.span(v.Index)
	vDur := vEnd - vStart
	if math.Abs(vDur-src.videoDur) > frame+eps {
		bad("video lasts %.3fs, source %.3fs (truncated or wrong length)", vDur, src.videoDur)
	}

	keys := p.keyframes(v.Index)
	maxGap := spec.KeyframeInterval.Seconds() + frame + eps
	switch {
	case len(keys) == 0:
		bad("no keyframes")
	case keys[0]-vStart > eps:
		bad("first keyframe at %.3fs, want %.3fs", keys[0], vStart)
	default:
		bounds := append(keys, vEnd)
		for i := 1; i < len(bounds); i++ {
			if gap := bounds[i] - bounds[i-1]; gap > maxGap {
				bad("keyframe gap of %.3fs at %.3fs, max %.3fs", gap, bounds[i-1], maxGap)
				break
			}
		}
	}

	if src.hasAudio {
		if p.audio == nil {
			bad("no audio stream, source has audio")
		} else {
			if p.audio.CodecName != "aac" {
				bad("audio codec %s, want aac", p.audio.CodecName)
			}
			aStart, aEnd := p.span(p.audio.Index)
			if diff := (aEnd - aStart) - src.audioDur; math.Abs(diff) > audioTolerance.Seconds() {
				bad("audio lasts %.3fs, source %.3fs (A/V drift)", aEnd-aStart, src.audioDur)
			}
		}
	}

	if len(problems) > 0 {
		return &InvalidError{problems}
	}
	return nil
}

type stream struct {
	Index        int    `json:"index"`
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Height       int    `json:"height"`
	PixFmt       string `json:"pix_fmt"`
	AvgFrameRate string `json:"avg_frame_rate"`
	Duration     string `json:"duration"`
	Profile      string `json:"profile"`
	Channels     int    `json:"channels"`
}

type packet struct {
	StreamIndex  int    `json:"stream_index"`
	PtsTime      string `json:"pts_time"`
	DurationTime string `json:"duration_time"`
	Flags        string `json:"flags"`
}

type probeOutput struct {
	Streams []stream `json:"streams"`
	Packets []packet `json:"packets"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(ctx context.Context, path string, entries string) (probeOutput, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", entries, "-of", "json", path)
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return probeOutput{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res probeOutput
	if err := json.Unmarshal(out, &res); err != nil {
		return probeOutput{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	return res, nil
}

type sourceInfo struct {
	height   int
	videoDur float64
	hasAudio bool
	audioDur float64
	audio    stream // first audio stream, if hasAudio
}

// probeSource reads stream metadata only; the source may be many GB.
func probeSource(ctx context.Context, path string) (sourceInfo, error) {
	res, err := ffprobe(ctx, path,
		"stream=index,codec_type,codec_name,profile,channels,height,duration:format=duration")
	if _, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		return sourceInfo{}, fmt.Errorf("%w: %w", ErrBadSource, err) // ffprobe ran and rejected the file
	}
	if err != nil {
		return sourceInfo{}, err
	}
	formatDur := parseFloat(res.Format.Duration)
	durOf := func(s stream) float64 {
		if d := parseFloat(s.Duration); d > 0 {
			return d
		}
		return formatDur // some containers (e.g. MKV) only record a total
	}

	var info sourceInfo
	var foundVideo bool
	for _, s := range res.Streams {
		switch {
		case s.CodecType == "video" && !foundVideo:
			foundVideo = true
			info.height = s.Height
			info.videoDur = durOf(s)
		case s.CodecType == "audio" && !info.hasAudio:
			info.hasAudio = true
			info.audioDur = durOf(s)
			info.audio = s
		}
	}
	if !foundVideo {
		return sourceInfo{}, fmt.Errorf("%w: no video stream", ErrBadSource)
	}
	return info, nil
}

type proxyInfo struct {
	video, audio *stream
	packets      []packet
}

// probeProxy reads stream metadata plus every packet's timing and flags.
// That demuxes the whole file but decodes nothing, so it stays fast.
func probeProxy(ctx context.Context, path string) (proxyInfo, error) {
	res, err := ffprobe(ctx, path,
		"stream=index,codec_type,codec_name,height,pix_fmt,avg_frame_rate"+
			":packet=stream_index,pts_time,duration_time,flags")
	if err != nil {
		return proxyInfo{}, err
	}
	info := proxyInfo{packets: res.Packets}
	for i := range res.Streams {
		s := &res.Streams[i]
		if s.CodecType == "video" && info.video == nil {
			info.video = s
		}
		if s.CodecType == "audio" && info.audio == nil {
			info.audio = s
		}
	}
	return info, nil
}

// span returns the earliest start and latest end of a stream's packets, in
// seconds. Packets arrive in decode order, so presentation times are not
// sorted when B-frames are present.
func (p proxyInfo) span(index int) (start, end float64) {
	start, end = math.Inf(1), math.Inf(-1)
	for _, pk := range p.packets {
		if pk.StreamIndex != index || pk.PtsTime == "" || pk.PtsTime == "N/A" {
			continue
		}
		pts := parseFloat(pk.PtsTime)
		start = min(start, pts)
		end = max(end, pts+parseFloat(pk.DurationTime))
	}
	if math.IsInf(start, 1) {
		return 0, 0
	}
	return start, end
}

// keyframes returns the sorted presentation times of a stream's keyframes.
func (p proxyInfo) keyframes(index int) []float64 {
	var keys []float64
	for _, pk := range p.packets {
		if pk.StreamIndex == index && strings.Contains(pk.Flags, "K") {
			keys = append(keys, parseFloat(pk.PtsTime))
		}
	}
	slices.Sort(keys)
	return keys
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parseRate parses an ffprobe rate such as "30000/1001".
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return parseFloat(s)
	}
	d := parseFloat(den)
	if d == 0 {
		return 0
	}
	return parseFloat(num) / d
}
