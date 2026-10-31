package hls

import (
	"strings"
	"testing"
)

// What ffmpeg's hls muxer writes for one chunk (-hls_segment_type fmp4).
const chunkPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.002000,
seg_000.m4s
#EXTINF:0.934267,
seg_001.m4s
#EXT-X-ENDLIST
`

func TestParse(t *testing.T) {
	m, err := Parse(chunkPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if m.Init != "init.mp4" || len(m.Segments) != 2 {
		t.Fatalf("got %+v", m)
	}
	if m.Segments[1] != (Segment{URI: "seg_001.m4s", Duration: 0.934267}) {
		t.Errorf("segment 1 = %+v", m.Segments[1])
	}
	if d := m.Duration(); d < 2.936 || d > 2.937 {
		t.Errorf("duration = %v, want 2.936267", d)
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"no map":          "#EXTM3U\n#EXTINF:2,\nseg.m4s\n",
		"no segments":     "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n",
		"uri no extinf":   "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\nseg.m4s\n",
		"bad duration":    "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:abc,\nseg.m4s\n",
		"two init maps":   "#EXTM3U\n#EXT-X-MAP:URI=\"a.mp4\"\n#EXT-X-MAP:URI=\"b.mp4\"\n#EXTINF:2,\nseg.m4s\n",
		"map with no uri": "#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"1@0\"\n#EXTINF:2,\nseg.m4s\n",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestVideoPlaylist(t *testing.T) {
	m, _ := Parse(chunkPlaylist)
	parts := []Part{{Dir: "v/0/1", Media: m}, {Dir: "v/1/3", Media: m}}

	running := VideoPlaylist(parts, false)
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:EVENT\n",
		"#EXT-X-TARGETDURATION:2\n",
		"#EXT-X-MAP:URI=\"v/0/1/init.mp4\"\n#EXTINF:2.002000,\nv/0/1/seg_000.m4s\n",
		"#EXT-X-MAP:URI=\"v/1/3/init.mp4\"\n",
	} {
		if !strings.Contains(running, want) {
			t.Errorf("playlist missing %q:\n%s", want, running)
		}
	}
	if strings.Contains(running, "ENDLIST") {
		t.Error("running playlist must not end")
	}
	if ended := VideoPlaylist(parts, true); !strings.HasSuffix(ended, "#EXT-X-ENDLIST\n") {
		t.Errorf("ended playlist has no ENDLIST:\n%s", ended)
	}
}

func TestTargetDurationRoundsLongestSegment(t *testing.T) {
	long := Media{Init: "i.mp4", Segments: []Segment{{URI: "a", Duration: 3.6}}}
	if got := VideoPlaylist([]Part{{Media: long}}, false); !strings.Contains(got, "#EXT-X-TARGETDURATION:4\n") {
		t.Errorf("want target 4:\n%s", got)
	}
}

func TestMasterPlaylist(t *testing.T) {
	withAudio := MasterPlaylist(2_000_000, true)
	for _, want := range []string{`URI="audio.m3u8"`, `CODECS="avc1.640028,mp4a.40.2"`, `AUDIO="audio"`, "\nvideo.m3u8\n"} {
		if !strings.Contains(withAudio, want) {
			t.Errorf("master missing %q:\n%s", want, withAudio)
		}
	}
	silent := MasterPlaylist(2_000_000, false)
	if strings.Contains(silent, "AUDIO") || strings.Contains(silent, "mp4a") {
		t.Errorf("silent master mentions audio:\n%s", silent)
	}
}
