package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// A minimal fMP4 box reader for the one thing ffprobe cannot be trusted on:
// where a browser will place a segment on the timeline. ffprobe applies edit
// lists, so a chunk whose media starts at 0 behind a 10 s empty edit probes
// as starting at 10 s. Media Source Extensions ignore edit lists and place
// each fragment at its tfdt (base media decode time), so that chunk would
// land on top of chunk 0 in a browser.

// fragmentTimeline is what a browser sees of one rendition: the track
// timescale, whether an edit list shifts it, and each segment's tfdt.
type fragmentTimeline struct {
	timescale uint32
	shifted   bool      // an empty edit (media_time -1) moves the media in time
	starts    []float64 // tfdt of each segment's first fragment, in seconds
}

func readTimeline(initPath string, segPaths []string) (fragmentTimeline, error) {
	b, err := os.ReadFile(initPath)
	if err != nil {
		return fragmentTimeline{}, err
	}
	var tl fragmentTimeline
	walkBoxes(b, func(typ string, body []byte) {
		switch typ {
		case "mdhd":
			if len(body) >= 24 && body[0] == 1 {
				tl.timescale = binary.BigEndian.Uint32(body[20:24])
			} else if len(body) >= 16 {
				tl.timescale = binary.BigEndian.Uint32(body[12:16])
			}
		case "elst":
			if len(body) < 8 {
				return
			}
			n := int(binary.BigEndian.Uint32(body[4:8]))
			for i := range n {
				var mediaTime int64
				if body[0] == 1 && len(body) >= 8+i*20+16 {
					mediaTime = int64(binary.BigEndian.Uint64(body[8+i*20+8:]))
				} else if len(body) >= 8+i*12+8 {
					mediaTime = int64(int32(binary.BigEndian.Uint32(body[8+i*12+4:])))
				}
				if mediaTime == -1 {
					tl.shifted = true
				}
			}
		}
	})
	if tl.timescale == 0 {
		return fragmentTimeline{}, errors.New("init segment has no mdhd timescale")
	}

	for _, p := range segPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			return fragmentTimeline{}, err
		}
		found := false
		walkBoxes(b, func(typ string, body []byte) {
			if typ != "tfdt" || found || len(body) < 8 {
				return
			}
			var t uint64
			if body[0] == 1 && len(body) >= 12 {
				t = binary.BigEndian.Uint64(body[4:12])
			} else {
				t = uint64(binary.BigEndian.Uint32(body[4:8]))
			}
			tl.starts = append(tl.starts, float64(t)/float64(tl.timescale))
			found = true
		})
		if !found {
			return fragmentTimeline{}, fmt.Errorf("%s: no tfdt", p)
		}
	}
	return tl, nil
}

// walkBoxes calls fn for every box in b, descending into the containers that
// lead to mdhd, elst and tfdt.
func walkBoxes(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b[0:4]))
		typ := string(b[4:8])
		hdr := uint64(8)
		switch size {
		case 0:
			size = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return
			}
			size, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		}
		if size < hdr || size > uint64(len(b)) {
			return
		}
		body := b[hdr:size]
		switch typ {
		case "moov", "trak", "mdia", "edts", "moof", "traf":
			walkBoxes(body, fn)
		default:
			fn(typ, body)
		}
		b = b[size:]
	}
}
