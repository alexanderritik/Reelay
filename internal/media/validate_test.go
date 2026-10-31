package media_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexanderritik/reelay/internal/media"
)

// encode runs ffmpeg on src with the given output options and returns the
// output path.
func encode(t *testing.T, src string, opts ...string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "proxy.mp4")
	args := append([]string{"-loglevel", "error", "-y", "-i", src}, opts...)
	args = append(args, dst)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v: %s", err, out)
	}
	return dst
}

// proxyOpts mirrors the real proxy settings; cases below break one thing.
var proxyOpts = []string{
	"-vf", "scale=-2:540", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	"-force_key_frames", "expr:gte(t,n_forced*1)", "-sc_threshold", "0", "-c:a", "aac",
	"-movflags", "+faststart",
}

// with returns proxyOpts with option name's value replaced, or removed when
// value is "", plus any extra options appended.
func with(name, value string, extra ...string) []string {
	opts := slices.Clone(proxyOpts)
	if i := slices.Index(opts, name); i >= 0 {
		if value == "" {
			opts = slices.Delete(opts, i, i+2)
		} else {
			opts[i+1] = value
		}
	}
	return append(opts, extra...)
}

func TestValidateAcceptsRealProxy(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "1920x1080", 3, true)
	proxy := filepath.Join(t.TempDir(), "proxy.mp4")
	if err := media.Transcode(context.Background(), src, proxy, media.DefaultProxy, nil); err != nil {
		t.Fatal(err)
	}
	if err := media.Validate(context.Background(), src, proxy, media.DefaultProxy); err != nil {
		t.Fatalf("real proxy rejected: %v", err)
	}
}

func TestValidateRejectsBadProxies(t *testing.T) {
	requireFFmpeg(t)
	src := makeSource(t, "1920x1080", 8, true)

	truncate := func(t *testing.T) string {
		good := encode(t, src, proxyOpts...)
		data, err := os.ReadFile(good)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(good, data[:len(data)*6/10], 0o644); err != nil {
			t.Fatal(err)
		}
		return good
	}
	garbage := func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "garbage.mp4")
		if err := os.WriteFile(p, []byte("not a video"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name  string
		make  func(t *testing.T) string
		wants []string // substrings expected among the problems
	}{
		// Faststart puts the header (with the full duration) first, so the file
		// still opens; only the packet-based length check can catch it.
		{"truncated at 60%", truncate, []string{"video lasts", "A/V drift"}},
		{"too short", func(t *testing.T) string { return encode(t, src, with("-t", "", "-t", "3")...) },
			[]string{"video lasts"}},
		{"5s keyframes", func(t *testing.T) string {
			return encode(t, src, with("-force_key_frames", "", "-g", "150", "-keyint_min", "150")...)
		}, []string{"keyframe gap"}},
		{"720p", func(t *testing.T) string { return encode(t, src, with("-vf", "scale=-2:720")...) },
			[]string{"height 720, want 540"}},
		{"10-bit", func(t *testing.T) string { return encode(t, src, with("-pix_fmt", "yuv420p10le")...) },
			[]string{"pixel format yuv420p10le"}},
		{"no audio", func(t *testing.T) string { return encode(t, src, with("-c:a", "", "-an")...) },
			[]string{"no audio stream"}},
		{"audio 2s short", func(t *testing.T) string { return encode(t, src, with("", "", "-af", "atrim=0:6")...) },
			[]string{"A/V drift"}},
		{"garbage bytes", garbage, []string{"cannot read proxy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := media.Validate(context.Background(), src, tt.make(t), media.DefaultProxy)
			invalid, ok := errors.AsType[*media.InvalidError](err)
			if !ok {
				t.Fatalf("err = %v, want *InvalidError", err)
			}
			t.Logf("problems: %q", invalid.Problems)
			for _, want := range tt.wants {
				if !strings.Contains(invalid.Error(), want) {
					t.Errorf("problems = %q, want one mentioning %q", invalid.Problems, want)
				}
			}
		})
	}
}
