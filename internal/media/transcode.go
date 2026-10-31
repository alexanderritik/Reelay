// Package media wraps the ffmpeg/ffprobe calls that make and check proxies.
package media

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// ProxySpec describes the playback proxy. Every setting serves smooth
// scrubbing in a browser editor.
type ProxySpec struct {
	Height           int           // output height; never upscaled
	KeyframeInterval time.Duration // fixed keyframe spacing, independent of frame rate
}

var DefaultProxy = ProxySpec{Height: 540, KeyframeInterval: time.Second}

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
