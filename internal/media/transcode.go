// Package media wraps the ffmpeg/ffprobe calls that make and check proxies.
package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ProxySpec describes the playback output.
type ProxySpec struct {
	Height           int           // single-file path: output height; never upscaled
	KeyframeInterval time.Duration // fixed keyframe spacing, independent of frame rate
	Ladder           []Rung        // chunked path: the renditions, lowest first
}

// Rung is one step of the quality ladder.
type Rung struct {
	Height  int // the short side in pixels, so portrait video gets the same rungs
	MaxRate int // bitrate cap in bits/s; the buffer is twice this
}

// Name is the rung's label, also its directory and playlist name.
func (r Rung) Name() string { return fmt.Sprintf("%dp", r.Height) }

// level is the H.264 level the encoder is pinned to. 4.0 covers up to
// 1080p30; above 720p it is 4.2, which covers 1080p60.
func (r Rung) level() string {
	if r.Height > 720 {
		return "4.2"
	}
	return "4.0"
}

// Codecs is the RFC 6381 string for the rung's video: H.264 High at its level.
func (r Rung) Codecs() string {
	if r.Height > 720 {
		return "avc1.64002a"
	}
	return "avc1.640028"
}

// DefaultLadder caps each rung's bitrate (CRF 23 under the cap) at roughly
// what it needs for most content.
var DefaultLadder = []Rung{
	{Height: 360, MaxRate: 800_000},
	{Height: 720, MaxRate: 2_800_000},
	{Height: 1080, MaxRate: 5_000_000},
}

var DefaultProxy = ProxySpec{Height: 540, KeyframeInterval: time.Second, Ladder: DefaultLadder}

// LadderFor picks the rungs for a source whose short side is short pixels.
// It never upscales: rungs above the source are dropped, and when the source
// falls between two rungs it gets one more at its own size (e.g. 478p gets
// 360p and 478p), unless that is within 20% of the rung below. A source
// smaller than every rung gets one rendition at its own size.
func (s ProxySpec) LadderFor(short int) []Rung {
	short &^= 1 // 4:2:0 needs even dimensions
	var out []Rung
	for _, r := range s.Ladder {
		if r.Height < short {
			out = append(out, r)
			continue
		}
		if len(out) == 0 || float64(short) >= 1.2*float64(out[len(out)-1].Height) {
			out = append(out, Rung{Height: short, MaxRate: r.MaxRate})
		}
		break
	}
	return out
}

// ParseLadder reads a ladder setting such as "360:800k,720:2800k,1080:5000k":
// short side and bitrate cap per rung, lowest first.
func ParseLadder(v string) ([]Rung, error) {
	var out []Rung
	for _, item := range strings.Split(v, ",") {
		h, rate, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok {
			return nil, fmt.Errorf("rung %q: want height:bitrate, e.g. 720:2800k", item)
		}
		height, err := strconv.Atoi(strings.TrimSuffix(h, "p"))
		if err != nil || height < 2 || height%2 != 0 {
			return nil, fmt.Errorf("rung %q: height must be an even number of pixels", item)
		}
		mult := 1
		switch {
		case strings.HasSuffix(rate, "k"):
			mult, rate = 1_000, strings.TrimSuffix(rate, "k")
		case strings.HasSuffix(rate, "M"):
			mult, rate = 1_000_000, strings.TrimSuffix(rate, "M")
		}
		n, err := strconv.Atoi(rate)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("rung %q: bad bitrate", item)
		}
		if len(out) > 0 && height <= out[len(out)-1].Height {
			return nil, fmt.Errorf("rung %q: rungs must go from lowest to highest", item)
		}
		out = append(out, Rung{Height: height, MaxRate: n * mult})
	}
	return out, nil
}

// Transcode writes a proxy of src to dst. Cancelling ctx kills ffmpeg, which
// is how a worker that lost its lease stops burning CPU. If onProgress is not
// nil it is called about twice a second with how much of the video has been
// encoded so far; it must not block.
func Transcode(ctx context.Context, src, dst string, spec ProxySpec, onProgress func(encoded time.Duration)) error {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-progress", "pipe:1", "-nostats", // machine-readable key=value progress on stdout
		"-i", src,
		"-map", "0:v:0", "-map", "0:a:0?", // first video stream, first audio if any
		"-vf", fmt.Sprintf("scale=-2:'min(%d,ih)'", spec.Height),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-pix_fmt", "yuv420p", // 8-bit 4:2:0 decodes in every browser
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%g)", spec.KeyframeInterval.Seconds()),
		"-sc_threshold", "0", // no extra scene-cut keyframes: spacing stays fixed
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-movflags", "+faststart", // moov atom first: playback starts before full download
		dst,
	}
	var report func(k, v string)
	if onProgress != nil {
		report = func(k, v string) {
			if k != "out_time_us" {
				return
			}
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 { // "N/A" before the first frame
				onProgress(time.Duration(us) * time.Microsecond)
			}
		}
	}
	return runFFmpeg(ctx, args, report)
}
