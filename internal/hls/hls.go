// Package hls describes chunked proxies as HLS: what one chunk published, and
// the playlists a player reads. It does no I/O; the worker fills it from
// ffmpeg's output and the API renders it from the job table.
package hls

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Media is one HLS rendition as published: an init segment and the media
// segments that follow it, as paths relative to its directory.
type Media struct {
	Init     string    `json:"init,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
}

type Segment struct {
	URI      string  `json:"uri"`
	Duration float64 `json:"duration"`        // seconds
	Bytes    int64   `json:"bytes,omitempty"` // size on disk; 0 if unknown
}

// Output is what one child job published, stored on its row when it
// commits. A video chunk publishes one Rendition per rung of the quality
// ladder, each in a subdirectory of the job's output key named after it.
// Audio publishes a single Media at the output key itself, and so did video
// chunks committed before the ladder existed.
type Output struct {
	Media
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Rendition is one rung of a video chunk's ladder: its media, and what a
// player needs to choose it.
type Rendition struct {
	Name    string `json:"name"` // e.g. "720p": its directory and playlist name
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Codecs  string `json:"codecs"`   // RFC 6381 video codec, e.g. avc1.640028
	MaxRate int    `json:"max_rate"` // the encoder's bitrate cap, bits/s
	Media
}

// Video returns the video renditions in o, lowest first. A chunk from
// before the ladder counts as one unnamed rendition at the output key.
func (o Output) Video() []Rendition {
	if len(o.Renditions) > 0 || o.Init == "" {
		return o.Renditions
	}
	return []Rendition{{Codecs: VideoCodec, Media: o.Media}}
}

// PlaylistURI is the media playlist a master playlist lists for the named
// rendition, next to the master so segment paths resolve the same way.
func PlaylistURI(name string) string {
	if name == "" {
		return "video.m3u8"
	}
	return "video_" + name + ".m3u8"
}

// Bitrates returns the peak and average bitrate of the given media in
// bits/s, from recorded segment sizes; zero when sizes are unknown.
// Segments under a second are left out of the peak: a chunk's last segment
// can be a few frames long, and a few frames' size over a tiny duration
// says nothing about the rate a player has to sustain.
func Bitrates(media []Media) (peak, avg int) {
	var bytes int64
	var dur float64
	for _, m := range media {
		for _, s := range m.Segments {
			if s.Bytes <= 0 || s.Duration <= 0 {
				continue
			}
			bytes += s.Bytes
			dur += s.Duration
			if s.Duration >= 1 {
				peak = max(peak, int(float64(s.Bytes*8)/s.Duration))
			}
		}
	}
	if dur == 0 {
		return 0, 0
	}
	avg = int(float64(bytes*8) / dur)
	return max(peak, avg), avg
}

// Duration is the total length of the segments.
func (m Media) Duration() float64 {
	var d float64
	for _, s := range m.Segments {
		d += s.Duration
	}
	return d
}

// Parse reads a media playlist written by ffmpeg's hls muxer. Only the tags
// ffmpeg emits for a single fMP4 rendition are understood.
func Parse(playlist string) (Media, error) {
	var m Media
	var pending *float64
	sc := bufio.NewScanner(strings.NewReader(playlist))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			uri, ok := attr(line, "URI")
			if !ok {
				return Media{}, fmt.Errorf("EXT-X-MAP without URI: %q", line)
			}
			if m.Init != "" {
				return Media{}, errors.New("more than one EXT-X-MAP")
			}
			m.Init = uri
		case strings.HasPrefix(line, "#EXTINF:"):
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			d, err := strconv.ParseFloat(v, 64)
			if err != nil || d <= 0 {
				return Media{}, fmt.Errorf("bad EXTINF: %q", line)
			}
			pending = &d
		case strings.HasPrefix(line, "#"):
		default:
			if pending == nil {
				return Media{}, fmt.Errorf("segment %q has no EXTINF", line)
			}
			m.Segments = append(m.Segments, Segment{URI: line, Duration: *pending})
			pending = nil
		}
	}
	if m.Init == "" {
		return Media{}, errors.New("no EXT-X-MAP (not an fMP4 playlist)")
	}
	if len(m.Segments) == 0 {
		return Media{}, errors.New("no segments")
	}
	return m, nil
}

// attr returns a quoted attribute value from a tag line.
func attr(line, name string) (string, bool) {
	_, rest, ok := strings.Cut(line, name+`="`)
	if !ok {
		return "", false
	}
	v, _, ok := strings.Cut(rest, `"`)
	return v, ok
}

// Part is one committed piece of the video playlist: a chunk's media and the
// directory its URIs are relative to.
type Part struct {
	Dir   string
	Media Media
}

// VideoPlaylist renders the video rendition from the chunks committed so
// far, in order. It is an EVENT playlist, so a player keeps polling it while
// the job runs; ended adds EXT-X-ENDLIST once every chunk is in. Callers
// must only ever pass a prefix that grows (chunks 0..k with no gaps), since
// an EVENT playlist may only be appended to.
//
// Each chunk was encoded by its own ffmpeg run, so each gets its own
// EXT-X-MAP: init segments are not byte-identical across runs.
func VideoPlaylist(parts []Part, ended bool) string {
	var b strings.Builder
	header(&b, targetDuration(parts))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for _, p := range parts {
		writeMedia(&b, p)
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// AudioPlaylist renders the audio rendition, which is encoded in one piece
// and so is complete as soon as it exists.
func AudioPlaylist(p Part) string {
	var b strings.Builder
	header(&b, targetDuration([]Part{p}))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	writeMedia(&b, p)
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// Codecs advertised in the master playlist. The chunk encoder pins the H.264
// profile and level so these strings are exact.
const (
	VideoCodec = "avc1.640028" // H.264 High, level 4.0; chunks from before the ladder
	AudioCodec = "mp4a.40.2"   // AAC-LC
)

// Variant is one entry of the master playlist: a video rendition a player
// may switch to.
type Variant struct {
	URI              string
	Bandwidth        int // peak bits/s, audio included
	AverageBandwidth int // 0 leaves it out
	Width, Height    int // 0 leaves RESOLUTION out
	Codecs           string
}

// MasterPlaylist lists the video variants, lowest first, and, if the source
// has audio, the one audio rendition they all share.
func MasterPlaylist(variants []Variant, hasAudio bool) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	audio := ""
	if hasAudio {
		b.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"` + "\n")
		audio = `,AUDIO="audio"`
	}
	for _, v := range variants {
		codecs := v.Codecs
		if hasAudio {
			codecs += "," + AudioCodec
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Bandwidth)
		if v.AverageBandwidth > 0 {
			fmt.Fprintf(&b, ",AVERAGE-BANDWIDTH=%d", v.AverageBandwidth)
		}
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		fmt.Fprintf(&b, ",CODECS=%q%s\n%s\n", codecs, audio, v.URI)
	}
	return b.String()
}

func header(b *strings.Builder, target int) {
	fmt.Fprintf(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-INDEPENDENT-SEGMENTS\n", target)
}

func writeMedia(b *strings.Builder, p Part) {
	fmt.Fprintf(b, "#EXT-X-MAP:URI=%q\n", join(p.Dir, p.Media.Init))
	for _, s := range p.Media.Segments {
		fmt.Fprintf(b, "#EXTINF:%.6f,\n%s\n", s.Duration, join(p.Dir, s.URI))
	}
}

// MinTargetDuration is the segment length the encoder aims for. The playlist
// never advertises less, so an EVENT playlist's target does not grow as the
// first chunks come in.
const MinTargetDuration = 2

// targetDuration is the longest segment rounded to the nearest second, as
// the HLS spec requires EXTINF to be no greater after rounding.
func targetDuration(parts []Part) int {
	t := MinTargetDuration
	for _, p := range parts {
		for _, s := range p.Media.Segments {
			t = max(t, int(math.Round(s.Duration)))
		}
	}
	return t
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}
