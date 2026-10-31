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

// Media is what one chunk or audio job published: an init segment and the
// media segments that follow it, as paths relative to the job's output
// directory. Stored on the job row when it commits.
type Media struct {
	Init     string    `json:"init"`
	Segments []Segment `json:"segments"`
}

type Segment struct {
	URI      string  `json:"uri"`
	Duration float64 `json:"duration"` // seconds
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
// profile and level so this string is exact.
const (
	VideoCodec = "avc1.640028" // H.264 High, level 4.0
	AudioCodec = "mp4a.40.2"   // AAC-LC
)

// MasterPlaylist points a player at the video rendition and, if the source
// has audio, the audio rendition. bandwidth is a peak estimate in bits/s.
// RESOLUTION is optional in HLS and left out: there is one rendition, so a
// player has nothing to choose between.
func MasterPlaylist(bandwidth int, hasAudio bool) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	codecs := VideoCodec
	audio := ""
	if hasAudio {
		b.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"` + "\n")
		codecs += "," + AudioCodec
		audio = `,AUDIO="audio"`
	}
	fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=%q%s\nvideo.m3u8\n", bandwidth, codecs, audio)
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
