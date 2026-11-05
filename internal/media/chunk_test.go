package media_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// and returns the plan, each chunk's output, and the source's frame count.
func encodeChunks(t *testing.T, src string, chunkLen float64) ([]media.Chunk, []media.ChunkOutput, int) {
	t.Helper()
	ctx := context.Background()
	frames, err := media.SourceFrames(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	chunks := media.PlanChunks(frames, chunkLen)
	var outs []media.ChunkOutput
	for i, c := range chunks {
		dir := filepath.Join(t.TempDir(), strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		o, err := media.TranscodeChunk(ctx, src, dir, media.DefaultProxy, c.Start, c.End, nil)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		for _, f := range media.OutputFiles(o) {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
				t.Errorf("chunk %d: listed file missing: %v", i, err)
			}
		}
		outs = append(outs, media.ChunkOutput{Dir: dir, HLS: o})
	}
	return chunks, outs, len(frames)
}

func TestChunksAssembleIntoExactProxy(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	src := makeCameraSource(t, 25)
	chunks, outs, nFrames := encodeChunks(t, src, 10)
	if len(chunks) != 3 {
		t.Fatalf("%d chunks, want 3", len(chunks))
	}

	for i, c := range chunks {
		if err := media.ValidateChunk(ctx, src, outs[i], media.DefaultProxy, c.Start, c.End, c.Frames); err != nil {
			t.Errorf("chunk %d: %v", i, err)
		}
	}
	if err := media.ValidateAssembly(ctx, src, outs, media.DefaultProxy, nFrames); err != nil {
		t.Errorf("assembly: %v", err)
	}
	// A 720p source gets 360p and 720p, never an upscaled 1080p.
	var got []string
	for _, r := range outs[0].HLS.Renditions {
		got = append(got, fmt.Sprintf("%s %dx%d", r.Name, r.Width, r.Height))
		for _, seg := range r.Segments {
			if seg.Bytes <= 0 {
				t.Errorf("%s %s: size not recorded", r.Name, seg.URI)
			}
		}
	}
	if want := []string{"360p 640x360", "720p 1280x720"}; !slices.Equal(got, want) {
		t.Errorf("renditions %v, want %v", got, want)
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
	chunks, outs, nFrames := encodeChunks(t, src, 10)
	c := chunks[1]

	wantInvalid := func(t *testing.T, err error, want string) {
		t.Helper()
		if _, ok := errors.AsType[*media.InvalidError](err); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want an media.InvalidError mentioning %q", err, want)
		}
	}

	// broken copies chunk i's output so a test can damage it.
	broken := func(t *testing.T, i int) media.ChunkOutput {
		dir := filepath.Join(t.TempDir(), "broken")
		copyDir(t, outs[i].Dir, dir)
		return media.ChunkOutput{Dir: dir, HLS: outs[i].HLS}
	}

	t.Run("frame count off by one", func(t *testing.T) {
		wantInvalid(t, media.ValidateChunk(ctx, src, outs[1], media.DefaultProxy, c.Start, c.End, c.Frames+1), "planned")
	})
	t.Run("wrong range", func(t *testing.T) {
		wantInvalid(t, media.ValidateChunk(ctx, src, outs[1], media.DefaultProxy, c.Start+1, c.End+1, c.Frames), "outside planned range")
	})
	t.Run("missing segment", func(t *testing.T) {
		b := broken(t, 2)
		if err := os.Remove(filepath.Join(b.Dir, "720p", "seg_001.m4s")); err != nil {
			t.Fatal(err)
		}
		last := chunks[2]
		wantInvalid(t, media.ValidateChunk(ctx, src, b, media.DefaultProxy, last.Start, last.End, last.Frames), "720p: ")
	})
	t.Run("one rung truncated", func(t *testing.T) {
		b := broken(t, 1)
		seg := filepath.Join(b.Dir, "720p", "seg_000.m4s")
		fi, err := os.Stat(seg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(seg, fi.Size()*6/10); err != nil {
			t.Fatal(err)
		}
		err = media.ValidateChunk(ctx, src, b, media.DefaultProxy, c.Start, c.End, c.Frames)
		wantInvalid(t, err, "720p: ")
		if strings.Contains(err.Error(), "360p") {
			t.Errorf("360p blamed too: %v", err)
		}
	})
	t.Run("missing rung", func(t *testing.T) {
		b := outs[1]
		b.HLS.Renditions = b.HLS.Renditions[:1] // published only 360p
		wantInvalid(t, media.ValidateChunk(ctx, src, b, media.DefaultProxy, c.Start, c.End, c.Frames), "rung is missing")
	})
	t.Run("seam gap when a chunk is left out", func(t *testing.T) {
		wantInvalid(t, media.ValidateAssembly(ctx, src, []media.ChunkOutput{outs[0], outs[2]}, media.DefaultProxy, nFrames), "gap")
	})
	t.Run("overlap when a chunk repeats", func(t *testing.T) {
		wantInvalid(t, media.ValidateAssembly(ctx, src, []media.ChunkOutput{outs[0], outs[1], outs[1], outs[2]}, media.DefaultProxy, nFrames), "overlap")
	})
	t.Run("chunks with different rungs", func(t *testing.T) {
		odd := outs[2]
		odd.HLS.Renditions = odd.HLS.Renditions[1:]
		wantInvalid(t, media.ValidateAssembly(ctx, src, []media.ChunkOutput{outs[0], outs[1], odd}, media.DefaultProxy, nFrames), "chunk 2: renditions")
	})
}

func TestLadderFor(t *testing.T) {
	for _, tc := range []struct {
		short int
		want  []string
	}{
		{2160, []string{"360p", "720p", "1080p"}}, // 4K: capped at the top rung
		{1080, []string{"360p", "720p", "1080p"}},
		{720, []string{"360p", "720p"}},
		{478, []string{"360p", "478p"}}, // between rungs: one more at its own size
		{479, []string{"360p", "478p"}}, // odd: rounded down to even
		{400, []string{"360p"}},         // within 20% of 360p: not worth a rung
		{240, []string{"240p"}},         // smaller than every rung
	} {
		if got := rungNamesOf(media.DefaultProxy.LadderFor(tc.short)); !slices.Equal(got, tc.want) {
			t.Errorf("LadderFor(%d) = %v, want %v", tc.short, got, tc.want)
		}
	}
	if r := media.DefaultProxy.LadderFor(478); r[1].MaxRate != 2_800_000 {
		t.Errorf("478p cap %d, want the 720p rung's", r[1].MaxRate)
	}
}

func rungNamesOf(rs []media.Rung) []string {
	var names []string
	for _, r := range rs {
		names = append(names, r.Name())
	}
	return names
}

func TestParseLadder(t *testing.T) {
	got, err := media.ParseLadder("360:800k, 720p:2800k,1080:5M")
	want := []media.Rung{{Height: 360, MaxRate: 800_000}, {Height: 720, MaxRate: 2_800_000}, {Height: 1080, MaxRate: 5_000_000}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("ParseLadder = %v, %v; want %v", got, err, want)
	}
	for _, bad := range []string{"", "720", "721:1M", "720:0", "720:2M,360:1M", "720:fast"} {
		if _, err := media.ParseLadder(bad); err == nil {
			t.Errorf("ParseLadder(%q) accepted", bad)
		}
	}
}

// A portrait phone video gets the same rungs by its short side, stored upright.
func TestPortraitSourceLadder(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	land := makeCameraSource(t, 4)
	src := filepath.Join(t.TempDir(), "portrait.mp4")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-display_rotation", "90",
		"-i", land, "-c", "copy", src).CombinedOutput(); err != nil {
		t.Fatalf("rotate: %v: %s", err, out)
	}
	chunks, outs, nFrames := encodeChunks(t, src, 10)
	var got []string
	for _, r := range outs[0].HLS.Renditions {
		got = append(got, fmt.Sprintf("%s %dx%d", r.Name, r.Width, r.Height))
	}
	if want := []string{"360p 360x640", "720p 720x1280"}; !slices.Equal(got, want) {
		t.Errorf("renditions %v, want %v", got, want)
	}
	c := chunks[0]
	if err := media.ValidateChunk(ctx, src, outs[0], media.DefaultProxy, c.Start, c.End, c.Frames); err != nil {
		t.Error(err)
	}
	if err := media.ValidateAssembly(ctx, src, outs, media.DefaultProxy, nFrames); err != nil {
		t.Error(err)
	}
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

			chunks, outs, nFrames := encodeChunks(t, src, 10)
			if len(chunks) != 1 {
				t.Fatalf("%d chunks, want 1", len(chunks))
			}
			c := chunks[0]
			if err := media.ValidateChunk(ctx, src, outs[0], media.DefaultProxy, c.Start, c.End, c.Frames); err != nil {
				t.Errorf("chunk: %v", err)
			}
			if err := media.ValidateAssembly(ctx, src, outs, media.DefaultProxy, nFrames); err != nil {
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
