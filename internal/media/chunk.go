package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/alexanderritik/reelay/internal/hls"
)

// PlaylistName is the media playlist ffmpeg writes next to a chunk's or the
// audio's segments. It is stored too, so validation can read what is stored.
const PlaylistName = "index.m3u8"

// segmentSeconds is the HLS segment length ffmpeg aims for. Segments are cut
// on keyframes, which come every KeyframeInterval.
const segmentSeconds = hls.MinTargetDuration

// TranscodeChunk encodes the video frames of src whose presentation time is
// in [start, end) as an fMP4 HLS rendition in dir, and returns what it wrote.
// Output timestamps are the source's own (-copyts), so chunks line up
// end to end without any stitching. If onFrames is not nil it gets the
// number of frames encoded so far, about twice a second; it must not block.
//
// Compared with the single-file proxy, chunks also turn off B-frames: decode
// order then equals display order, so a seam can never need a frame from the
// previous chunk, and a scrubbing editor decodes fewer frames per seek. The
// H.264 profile and level are pinned to match hls.VideoCodec.
func TranscodeChunk(ctx context.Context, src, dir string, spec ProxySpec, start, end float64, onFrames func(int)) (hls.Media, error) {
	ts := func(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-progress", "pipe:1", "-nostats",
		"-stats_period", "0.25", // chunks are short: report often enough to show
		// Seek to the keyframe at or before start (fast), keep source time,
		// then trim to exactly [start, end) on presentation timestamps.
		"-noaccurate_seek", "-ss", ts(start), "-i", src, "-copyts",
		"-map", "0:v:0", "-an",
		"-vf", fmt.Sprintf("trim=start=%s:end=%s,scale=-2:'min(%d,ih)'", ts(start), ts(end), spec.Height),
		"-fps_mode", "passthrough", // one output frame per source frame: counts must match the plan
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-profile:v", "high", "-level:v", "4.0", "-bf", "0",
		"-pix_fmt", "yuv420p",
		// t counts from the chunk's first frame, which sits on the grid.
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%g)", spec.KeyframeInterval.Seconds()),
		"-sc_threshold", "0",
	}
	// frag_discont writes each fragment's tfdt from the source timestamps.
	// Without it the muxer starts the media at 0 and records the offset in an
	// edit list, which ffprobe honours but browsers ignore (see mp4box.go).
	args = append(args, "-hls_segment_options", "movflags=+frag_discont")
	args = append(args, hlsOutput(dir)...)

	var report func(k, v string)
	if onFrames != nil {
		report = func(k, v string) {
			if k != "frame" {
				return
			}
			if n, err := strconv.Atoi(v); err == nil {
				onFrames(n)
			}
		}
	}
	if err := runFFmpeg(ctx, args, report); err != nil {
		return hls.Media{}, err
	}
	return readPlaylist(dir)
}

// TranscodeAudio writes the whole audio track of src as an fMP4 HLS
// rendition in dir. Audio is never chunked: every AAC encode starts with
// priming samples, so joined chunks would click or drift at each seam.
//
// Playback cannot start until audio is in, so it must be quick. Source audio
// that browsers already play (AAC-LC, mono or stereo, as phones and most
// cameras record) is copied, which takes well under a second; anything else
// is encoded with the fast AAC coder, about twice as fast as the default and
// plenty for scrubbing.
func TranscodeAudio(ctx context.Context, src, dir string) (hls.Media, error) {
	s, err := probeSource(ctx, src)
	if err != nil {
		return hls.Media{}, err
	}
	codec := []string{"-c:a", "aac", "-aac_coder", "fast", "-b:a", "128k", "-ac", "2"}
	if a := s.audio; a.CodecName == "aac" && a.Profile == "LC" && a.Channels <= 2 {
		codec = []string{"-c:a", "copy"}
	}
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-progress", "pipe:1", "-nostats",
		"-i", src,
		"-map", "0:a:0", "-vn",
	}
	args = append(args, codec...)
	args = append(args, hlsOutput(dir)...)
	if err := runFFmpeg(ctx, args, nil); err != nil {
		return hls.Media{}, err
	}
	return readPlaylist(dir)
}

func hlsOutput(dir string) []string {
	return []string{
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", strconv.Itoa(segmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(dir, "seg_%03d.m4s"),
		filepath.Join(dir, PlaylistName),
	}
}

func readPlaylist(dir string) (hls.Media, error) {
	b, err := os.ReadFile(filepath.Join(dir, PlaylistName))
	if err != nil {
		return hls.Media{}, err
	}
	m, err := hls.Parse(string(b))
	if err != nil {
		return hls.Media{}, fmt.Errorf("ffmpeg playlist: %w", err)
	}
	return m, nil
}

// Files lists every file of a rendition, playlist included, relative to its
// directory: what must be stored for it.
func Files(m hls.Media) []string {
	files := []string{PlaylistName, m.Init}
	for _, s := range m.Segments {
		files = append(files, s.URI)
	}
	return files
}
