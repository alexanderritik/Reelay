package media_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexanderritik/reelay/internal/media"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// makeSource renders a synthetic test video with ffmpeg's lavfi sources.
func makeSource(t *testing.T, size string, seconds int, audio bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=30"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440")
	}
	args = append(args, "-t", strconv.Itoa(seconds),
		"-c:v", "libx264", "-preset", "ultrafast")
	if audio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("make source: %v: %s", err, out)
	}
	return path
}

type probeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Height    int    `json:"height"`
	PixFmt    string `json:"pix_fmt"`
}

func probeStreams(t *testing.T, path string) []probeStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var res struct{ Streams []probeStream }
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	return res.Streams
}

func TestTranscodeMakesBrowserProxy(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "1920x1080", 3, true)
	dst := filepath.Join(t.TempDir(), "proxy.mp4")

	if err := media.Transcode(context.Background(), src, dst, media.DefaultProxy, nil); err != nil {
		t.Fatal(err)
	}

	var gotVideo, gotAudio bool
	for _, s := range probeStreams(t, dst) {
		switch s.CodecType {
		case "video":
			gotVideo = true
			if s.CodecName != "h264" || s.Height != 540 || s.PixFmt != "yuv420p" {
				t.Errorf("video = %+v, want h264 540p yuv420p", s)
			}
		case "audio":
			gotAudio = true
			if s.CodecName != "aac" {
				t.Errorf("audio codec = %s, want aac", s.CodecName)
			}
		}
	}
	if !gotVideo || !gotAudio {
		t.Errorf("video=%v audio=%v, want both", gotVideo, gotAudio)
	}
}

func TestTranscodeWithoutAudio(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "640x360", 2, false)
	dst := filepath.Join(t.TempDir(), "proxy.mp4")

	if err := media.Transcode(context.Background(), src, dst, media.DefaultProxy, nil); err != nil {
		t.Fatal(err)
	}
	streams := probeStreams(t, dst)
	if len(streams) != 1 || streams[0].CodecType != "video" {
		t.Fatalf("streams = %+v, want one video stream", streams)
	}
	if streams[0].Height != 360 {
		t.Errorf("height = %d, want 360 (no upscaling)", streams[0].Height)
	}
}

func TestTranscodeStopsWhenCancelled(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "1920x1080", 30, true)
	dst := filepath.Join(t.TempDir(), "proxy.mp4")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := media.Transcode(ctx, src, dst, media.DefaultProxy, nil)
	if err == nil {
		t.Fatal("transcode finished before cancel; make the source longer")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v, want a cancellation error", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("returned after %v, want ffmpeg killed promptly", took)
	}
}

func TestTranscodeReportsFFmpegError(t *testing.T) {
	requireFFmpeg(t)
	src := filepath.Join(t.TempDir(), "garbage.mp4")
	if err := os.WriteFile(src, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := media.Transcode(context.Background(), src, filepath.Join(t.TempDir(), "out.mp4"), media.DefaultProxy, nil)
	if err == nil {
		t.Fatal("want an error for a non-video input")
	}
	if !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("err = %v, want ffmpeg's own message included", err)
	}
}

func TestTranscodeReportsProgress(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "1920x1080", 6, true)
	dst := filepath.Join(t.TempDir(), "proxy.mp4")

	var reports []time.Duration // only appended from the reading loop, no lock needed
	if err := media.Transcode(context.Background(), src, dst, media.DefaultProxy,
		func(d time.Duration) { reports = append(reports, d) }); err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 {
		t.Fatal("no progress reported")
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
	if last := reports[len(reports)-1]; last < 5500*time.Millisecond || last > 6500*time.Millisecond {
		t.Errorf("final progress %v, want about 6s (the whole source)", last)
	}
}

func TestCheckSourceReturnsDuration(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "640x360", 3, false)
	d, err := media.CheckSource(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if d < 2900*time.Millisecond || d > 3100*time.Millisecond {
		t.Errorf("duration = %v, want about 3s", d)
	}
}
