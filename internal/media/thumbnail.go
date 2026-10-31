package media

import (
	"context"
	"fmt"
	"strconv"
)

// Thumbnail writes one JPEG frame of src, taken at `at` seconds, to dst,
// scaled to the given width. It seeks on the input, so it costs one decode
// from the nearest keyframe, not a pass over the file.
func Thumbnail(ctx context.Context, src, dst string, at float64, width int) error {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-progress", "pipe:1", "-nostats",
		"-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", src,
		"-map", "0:v:0", "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-q:v", "4",
		dst,
	}
	return runFFmpeg(ctx, args, nil)
}
