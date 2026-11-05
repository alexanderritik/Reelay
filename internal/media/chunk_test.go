package media_test

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexanderritik/reelay/internal/media"
)

func ntscFrames(seconds float64) []float64 {
	var f []float64
	for i := 0; float64(i)*1001/30000 < seconds; i++ {
		f = append(f, float64(i)*1001/30000)
	}
	return f
}

func TestPlanChunksCoversEveryFrameOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []float64
	}{
		{"29.97fps", ntscFrames(35)},
		{"30fps, frame exactly on the grid", func() []float64 {
			var f []float64
			for i := range 30 * 25 {
				f = append(f, float64(i)/30)
			}
			return f
		}()},
		{"single frame", []float64{0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := media.PlanChunks(tc.frames, 10)
			total := 0
			for i, c := range chunks {
				total += c.Frames
				if i > 0 && c.Start != chunks[i-1].End {
					t.Errorf("chunk %d starts %v, previous ends %v", i, c.Start, chunks[i-1].End)
				}
				// Recount from the boundaries, as trim would.
				n := 0
				for _, f := range tc.frames {
					if f >= c.Start && f < c.End {
						n++
					}
					if math.Abs(f-c.End) < 1e-4 || math.Abs(f-c.Start) < 1e-4 && f != 0 {
						t.Errorf("frame %v sits on a boundary of chunk %d [%v, %v)", f, i, c.Start, c.End)
					}
				}
				if n != c.Frames {
					t.Errorf("chunk %d [%v, %v) holds %d frames, planned %d", i, c.Start, c.End, n, c.Frames)
				}
			}
			if total != len(tc.frames) {
				t.Errorf("chunks hold %d frames, source has %d", total, len(tc.frames))
			}
		})
	}

	if got := len(media.PlanChunks(ntscFrames(35), 10)); got != 4 {
		t.Errorf("35s in 10s chunks = %d chunks, want 4", got)
	}
}

// makeCameraSource renders a source like real camera footage: 29.97 fps,
// B-frames and keyframes 2.5s apart, so chunk starts fall between keyframes.
func makeCameraSource(t *testing.T, seconds int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "camera.mp4")
	out, err := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30000/1001",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(seconds),
		"-c:v", "libx264", "-preset", "veryfast", "-g", "75", "-bf", "3", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Fatalf("make source: %v: %s", err, out)
	}
	return path
}

// encodeChunks plans and encodes src into chunk directories under a temp dir
// and returns the plan and each chunk's playlist path.
func encodeChunks(t *testing.T, src string, chunkLen float64) ([]media.Chunk, []string, int) {
	t.Helper()
	ctx := context.Background()
	frames, err := media.SourceFrames(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	chunks := media.PlanChunks(frames, chunkLen)
	var playlists []string
	for i, c := range chunks {
		dir := filepath.Join(t.TempDir(), strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		m, err := media.TranscodeChunk(ctx, src, dir, media.DefaultProxy, c.Start, c.End, nil)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		for _, f := range media.Files(m) {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("chunk %d: listed file missing: %v", i, err)
			}
		}
		playlists = append(playlists, filepath.Join(dir, media.PlaylistName))
	}
	return chunks, playlists, len(frames)
}

func TestChunksAssembleIntoExactProxy(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	src := makeCameraSource(t, 25)
	chunks, playlists, nFrames := encodeChunks(t, src, 10)
	if len(chunks) != 3 {
		t.Fatalf("%d chunks, want 3", len(chunks))
	}

	for i, c := range chunks {
		if err := media.ValidateChunk(ctx, src, playlists[i], media.DefaultProxy, c.Start, c.End, c.Frames); err != nil {
			t.Errorf("chunk %d: %v", i, err)
		}
	}
	if err := media.ValidateAssembly(ctx, src, playlists, media.DefaultProxy, nFrames); err != nil {
		t.Errorf("assembly: %v", err)
	}

	audioDir := t.TempDir()
	if _, err := media.TranscodeAudio(ctx, src, audioDir); err != nil {
		t.Fatal(err)
	}
	if err := media.ValidateAudio(ctx, src, filepath.Join(audioDir, media.PlaylistName)); err != nil {
		t.Errorf("audio: %v", err)
	}
}

func TestValidateCatchesBadChunks(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	src := makeCameraSource(t, 25)
	chunks, playlists, nFrames := encodeChunks(t, src, 10)
	c := chunks[1]

	wantInvalid := func(t *testing.T, err error, want string) {
		t.Helper()
		if _, ok := errors.AsType[*media.InvalidError](err); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want an media.InvalidError mentioning %q", err, want)
		}
	}

	t.Run("frame count off by one", func(t *testing.T) {
		wantInvalid(t, media.ValidateChunk(ctx, src, playlists[1], media.DefaultProxy, c.Start, c.End, c.Frames+1), "planned")
	})
	t.Run("wrong range", func(t *testing.T) {
		wantInvalid(t, media.ValidateChunk(ctx, src, playlists[1], media.DefaultProxy, c.Start+1, c.End+1, c.Frames), "outside planned range")
	})
	t.Run("missing segment", func(t *testing.T) {
		dir := filepath.Dir(playlists[2])
		broken := filepath.Join(t.TempDir(), "broken")
		copyDir(t, dir, broken)
		if err := os.Remove(filepath.Join(broken, "seg_001.m4s")); err != nil {
			t.Fatal(err)
		}
		last := chunks[2]
		wantInvalid(t, media.ValidateChunk(ctx, src, filepath.Join(broken, media.PlaylistName), media.DefaultProxy, last.Start, last.End, last.Frames), "")
	})
	t.Run("seam gap when a chunk is left out", func(t *testing.T) {
		wantInvalid(t, media.ValidateAssembly(ctx, src, []string{playlists[0], playlists[2]}, media.DefaultProxy, nFrames), "gap")
	})
	t.Run("overlap when a chunk repeats", func(t *testing.T) {
		wantInvalid(t, media.ValidateAssembly(ctx, src, []string{playlists[0], playlists[1], playlists[1], playlists[2]}, media.DefaultProxy, nFrames), "overlap")
	})
}

func TestTranscodeChunkReportsFrames(t *testing.T) {
	requireFFmpeg(t)
	src := makeCameraSource(t, 12)
	var last int
	_, err := media.TranscodeChunk(context.Background(), src, t.TempDir(), media.DefaultProxy, 0, 10.5, func(n int) { last = n })
	if err != nil {
		t.Fatal(err)
	}
	if last < 300 || last > 320 {
		t.Errorf("last frame report %d, want about 315", last)
	}
}

func TestTranscodeChunkStopsWhenCancelled(t *testing.T) {
	requireFFmpeg(t)
	src := makeCameraSource(t, 12)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := media.TranscodeChunk(ctx, src, t.TempDir(), media.DefaultProxy, 0, 12, nil); err == nil {
		t.Error("no error after cancel")
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		t.Fatal(err)
	}
}

func TestTranscodeAudioCopiesBrowserReadyAAC(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	src := makeCameraSource(t, 12) // AAC-LC mono
	dir := t.TempDir()
	start := time.Now()
	if _, err := media.TranscodeAudio(ctx, src, dir); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("audio took %v; AAC-LC source should be copied, not encoded", took)
	}
	if err := media.ValidateAudio(ctx, src, filepath.Join(dir, media.PlaylistName)); err != nil {
		t.Error(err)
	}
}

func TestTranscodeAudioEncodesOtherCodecs(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "opus.mkv")
	out, err := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "libopus", src).CombinedOutput()
	if err != nil {
		t.Skipf("no libopus: %v: %s", err, out)
	}
	dir := t.TempDir()
	if _, err := media.TranscodeAudio(ctx, src, dir); err != nil {
		t.Fatal(err)
	}
	if err := media.ValidateAudio(ctx, src, filepath.Join(dir, media.PlaylistName)); err != nil {
		t.Error(err)
	}
}

// Every upload takes the chunked path, so the shortest clips must too: a
// source shorter than one chunk becomes a single chunk that validates and
// assembles like any other.
func TestShortSourcesTakeChunkedPath(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		input []string // ffmpeg output options that shape the clip
		audio bool
	}{
		{"3 seconds", []string{"-t", "3"}, true},
		{"1 second", []string{"-t", "1"}, true},
		{"half a second", []string{"-t", "0.5"}, true},
		{"single frame", []string{"-frames:v", "1"}, false},
		{"no audio", []string{"-t", "3"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "short.mp4")
			args := []string{"-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30000/1001"}
			if tc.audio {
				args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-c:a", "aac", "-shortest")
			}
			args = append(args, tc.input...)
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-bf", "3", "-pix_fmt", "yuv420p", src)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("make source: %v: %s", err, out)
			}

			chunks, playlists, nFrames := encodeChunks(t, src, 10)
			if len(chunks) != 1 {
				t.Fatalf("%d chunks, want 1", len(chunks))
			}
			c := chunks[0]
			if err := media.ValidateChunk(ctx, src, playlists[0], media.DefaultProxy, c.Start, c.End, c.Frames); err != nil {
				t.Errorf("chunk: %v", err)
			}
			if err := media.ValidateAssembly(ctx, src, playlists, media.DefaultProxy, nFrames); err != nil {
				t.Errorf("assembly: %v", err)
			}
			if !tc.audio {
				return
			}
			audioDir := t.TempDir()
			if _, err := media.TranscodeAudio(ctx, src, audioDir); err != nil {
				t.Fatal(err)
			}
			if err := media.ValidateAudio(ctx, src, filepath.Join(audioDir, media.PlaylistName)); err != nil {
				t.Errorf("audio: %v", err)
			}
		})
	}
}
