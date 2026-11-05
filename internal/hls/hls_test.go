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
	variants := []Variant{
		{URI: PlaylistURI("360p"), Bandwidth: 900_000, AverageBandwidth: 600_000, Width: 640, Height: 360, Codecs: "avc1.640028"},
		{URI: PlaylistURI("1080p"), Bandwidth: 5_200_000, Width: 1920, Height: 1080, Codecs: "avc1.64002a"},
	}
	withAudio := MasterPlaylist(variants, true)
	for _, want := range []string{
		`URI="audio.m3u8"`,
		"#EXT-X-STREAM-INF:BANDWIDTH=900000,AVERAGE-BANDWIDTH=600000,RESOLUTION=640x360," +
			`CODECS="avc1.640028,mp4a.40.2",AUDIO="audio"` + "\nvideo_360p.m3u8\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=5200000,RESOLUTION=1920x1080," +
			`CODECS="avc1.64002a,mp4a.40.2",AUDIO="audio"` + "\nvideo_1080p.m3u8\n",
	} {
		if !strings.Contains(withAudio, want) {
			t.Errorf("master missing %q:\n%s", want, withAudio)
		}
	}
	if strings.Index(withAudio, "video_360p") > strings.Index(withAudio, "video_1080p") {
		t.Errorf("variants out of order:\n%s", withAudio)
	}
	silent := MasterPlaylist(variants, false)
	if strings.Contains(silent, "AUDIO") || strings.Contains(silent, "mp4a") {
		t.Errorf("silent master mentions audio:\n%s", silent)
	}
}

func TestBitrates(t *testing.T) {
	m := []Media{
		{Segments: []Segment{{URI: "a", Duration: 2, Bytes: 250_000}, {URI: "b", Duration: 2, Bytes: 500_000}}},
		// A 3-frame tail: huge per-second rate, but too short to count as the peak.
		{Segments: []Segment{{URI: "c", Duration: 0.1, Bytes: 100_000}}},
	}
	peak, avg := Bitrates(m)
	if peak != 2_000_000 {
		t.Errorf("peak = %d, want 2000000 (500 kB over 2 s)", peak)
	}
	if want := 1_658_536; avg != want { // 850 kB over 4.1 s
		t.Errorf("avg = %d, want %d", avg, want)
	}
	if p, a := Bitrates([]Media{{Segments: []Segment{{URI: "x", Duration: 2}}}}); p != 0 || a != 0 {
		t.Errorf("unknown sizes gave %d, %d; want 0, 0", p, a)
	}
}

func TestOutputVideo(t *testing.T) {
	legacy := Output{Media: Media{Init: "init.mp4", Segments: []Segment{{URI: "s", Duration: 2}}}}
	if v := legacy.Video(); len(v) != 1 || v[0].Name != "" || v[0].Init != "init.mp4" {
		t.Errorf("pre-ladder chunk: %+v, want one unnamed rendition", v)
	}
	ladder := Output{Renditions: []Rendition{{Name: "360p"}, {Name: "720p"}}}
	if v := ladder.Video(); len(v) != 2 || v[1].Name != "720p" {
		t.Errorf("ladder chunk: %+v", v)
	}
	if PlaylistURI("") != "video.m3u8" || PlaylistURI("720p") != "video_720p.m3u8" {
		t.Error("PlaylistURI")
	}
}
