package api

// Playback of chunked proxies. Playlists are not stored: every request
// renders them from the job table, which is the only source of truth, so
// concurrent chunk commits can never race to write a playlist file, and a
// segment appears only after its fenced commit. At high request rates this
// is the part to cache.
//
//	GET /jobs/{id}/hls/master.m3u8         one variant per rung, plus audio
//	GET /jobs/{id}/hls/video_{rung}.m3u8   EVENT playlist for one rung, grows as chunks commit
//	GET /jobs/{id}/hls/video.m3u8          the top rung (and chunks from before the ladder)
//	GET /jobs/{id}/hls/audio.m3u8          the audio rendition
//	GET /jobs/{id}/hls/{file...}           init and media segments

import (
	"errors"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/alexanderritik/reelay/internal/hls"
	"github.com/alexanderritik/reelay/internal/jobs"
)

// legacyBandwidth is advertised for a video from before the ladder, whose
// one 540p rendition has no recorded segment sizes.
const legacyBandwidth = 3_000_000

// audioBandwidth is assumed for audio whose segment sizes are unknown: the
// 128 kbit/s it is encoded at, plus container overhead.
const audioBandwidth = 160_000

// graph is a chunked job as the playlists see it.
type graph struct {
	root   jobs.Job
	chunks []jobs.Job // in index order
	audio  *jobs.Job
}

// renditions are the video rungs a player may choose from, lowest first:
// those chunk 0 published. Every chunk of a video publishes the same rungs
// (finalize checks it), so chunk 0 speaks for the rest.
func (g graph) renditions() []hls.Rendition {
	if len(g.chunks) == 0 || g.chunks[0].State != jobs.StateDone || g.chunks[0].HLS == nil {
		return nil
	}
	return g.chunks[0].HLS.Video()
}

// playable is one rung's run of committed chunks from chunk 0, stopping at
// the first one not yet done. It only ever grows, because done is final,
// which is exactly what an EVENT playlist allows.
func (g graph) playable(rung string) []hls.Part {
	var parts []hls.Part
	for _, c := range g.chunks {
		if c.State != jobs.StateDone || c.HLS == nil {
			break
		}
		p, ok := part(g.root.ID, c, rung)
		if !ok {
			break
		}
		parts = append(parts, p)
	}
	return parts
}

// ready reports whether a player can start: audio (if any) and chunk 0 are in.
func (g graph) ready() bool {
	if g.audio != nil && g.audio.State != jobs.StateDone {
		return false
	}
	return len(g.renditions()) > 0
}

// part is the named rung of a committed chunk, with its directory relative
// to the job's prefix.
func part(rootID string, child jobs.Job, rung string) (hls.Part, bool) {
	dir := strings.TrimPrefix(*child.OutputKey, "proxies/"+rootID+"/")
	for _, r := range child.HLS.Video() {
		if r.Name == rung {
			if rung != "" {
				dir += "/" + rung
			}
			return hls.Part{Dir: dir, Media: r.Media}, true
		}
	}
	return hls.Part{}, false
}

func audioPart(rootID string, child jobs.Job) hls.Part {
	return hls.Part{
		Dir:   strings.TrimPrefix(*child.OutputKey, "proxies/"+rootID+"/"),
		Media: child.HLS.Media,
	}
}

// variants describes each rung for the master playlist. BANDWIDTH is the peak
// a player must sustain: the higher of the peak measured over the chunks so
// far and the rung's encoder cap (which holds for chunks not yet encoded),
// plus the audio's. AVERAGE-BANDWIDTH is measured, so it firms up as chunks
// come in.
func (g graph) variants() []hls.Variant {
	aPeak, aAvg := 0, 0
	if g.audio != nil {
		aPeak, aAvg = hls.Bitrates([]hls.Media{g.audio.HLS.Media})
		if aPeak == 0 {
			aPeak, aAvg = audioBandwidth, audioBandwidth
		}
	}
	var out []hls.Variant
	for _, r := range g.renditions() {
		var media []hls.Media
		for _, p := range g.playable(r.Name) {
			media = append(media, p.Media)
		}
		peak, avg := hls.Bitrates(media)
		v := hls.Variant{
			URI: hls.PlaylistURI(r.Name), Width: r.Width, Height: r.Height, Codecs: r.Codecs,
			Bandwidth: max(peak, r.MaxRate) + aPeak,
		}
		if avg > 0 {
			v.AverageBandwidth = avg + aAvg
		}
		if peak == 0 && r.MaxRate == 0 {
			v.Bandwidth = legacyBandwidth
		}
		out = append(out, v)
	}
	return out
}

// loadGraph fetches a chunked job and its children, writing the error
// response itself when there is nothing to play yet.
func (s *Server) loadGraph(w http.ResponseWriter, r *http.Request) (graph, bool) {
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return graph{}, false
	}
	root, err := s.Jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return graph{}, false
	}
	if err != nil {
		s.internalError(w, "get job", err)
		return graph{}, false
	}
	switch {
	case root.Kind != jobs.KindProxy:
		writeError(w, http.StatusNotFound, "not an upload's job")
		return graph{}, false
	case root.State == jobs.StateFailed:
		writeError(w, http.StatusGone, "job failed")
		return graph{}, false
	case root.Chunks == nil && root.State == jobs.StateDone:
		writeError(w, http.StatusNotFound, "single-file proxy: play output_key instead")
		return graph{}, false
	case root.Chunks == nil:
		notReady(w)
		return graph{}, false
	}

	kids, err := s.Jobs.Children(r.Context(), id)
	if err != nil {
		s.internalError(w, "get children", err)
		return graph{}, false
	}
	g := graph{root: root}
	for i := range kids {
		switch kids[i].Kind {
		case jobs.KindChunk:
			g.chunks = append(g.chunks, kids[i])
		case jobs.KindAudio:
			g.audio = &kids[i]
		}
	}
	if !g.ready() {
		notReady(w)
		return graph{}, false
	}
	return g, true
}

func notReady(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusNotFound, "not ready: waiting for the first chunk and audio")
}

func (s *Server) masterPlaylist(w http.ResponseWriter, r *http.Request) {
	g, ok := s.loadGraph(w, r)
	if !ok {
		return
	}
	// Bandwidth figures are measured from the chunks so far, so a player
	// that refetches it mid-encode gets fresher ones.
	writePlaylist(w, hls.MasterPlaylist(g.variants(), g.audio != nil), len(g.playable(g.top())) < len(g.chunks))
}

// top is the highest rung's name: what video.m3u8 serves.
func (g graph) top() string {
	rs := g.renditions()
	return rs[len(rs)-1].Name
}

// videoPlaylist serves video.m3u8, the top rung.
func (s *Server) videoPlaylist(w http.ResponseWriter, r *http.Request) {
	g, ok := s.loadGraph(w, r)
	if !ok {
		return
	}
	s.writeRung(w, g, g.top())
}

// rungPlaylist serves video_{rung}.m3u8.
func (s *Server) rungPlaylist(w http.ResponseWriter, r *http.Request, rung string) {
	g, ok := s.loadGraph(w, r)
	if !ok {
		return
	}
	for _, rd := range g.renditions() {
		if rd.Name == rung && rung != "" {
			s.writeRung(w, g, rung)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such rendition")
}

func (s *Server) writeRung(w http.ResponseWriter, g graph, rung string) {
	parts := g.playable(rung)
	ended := len(parts) == len(g.chunks)
	// While it grows, the player must refetch it; once ended it is final.
	writePlaylist(w, hls.VideoPlaylist(parts, ended), !ended)
}

func (s *Server) audioPlaylist(w http.ResponseWriter, r *http.Request) {
	g, ok := s.loadGraph(w, r)
	if !ok {
		return
	}
	if g.audio == nil {
		writeError(w, http.StatusNotFound, "source has no audio")
		return
	}
	writePlaylist(w, hls.AudioPlaylist(audioPart(g.root.ID, *g.audio)), false)
}

func writePlaylist(w http.ResponseWriter, body string, live bool) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if live {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = io.WriteString(w, body)
}

var segmentTypes = map[string]string{
	".mp4": "video/mp4",         // init segments
	".m4s": "video/iso.segment", // media segments
}

// segment serves an init or media segment from the job's output prefix. Every
// path includes the fence of the commit that wrote it and is never rewritten,
// so segments can be cached forever.
func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	id, file := r.PathValue("id"), r.PathValue("file")
	if rung, ok := strings.CutPrefix(file, "video_"); ok && !strings.Contains(file, "/") {
		if rung, ok := strings.CutSuffix(rung, ".m3u8"); ok {
			s.rungPlaylist(w, r, rung)
			return
		}
	}
	ctype, ok := segmentTypes[path.Ext(file)]
	if uuid.Validate(id) != nil || !ok || !filepath.IsLocal(file) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	f, err := s.Storage.Open(r.Context(), path.Join("proxies", id, file))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs) // supports Range requests
		return
	}
	_, _ = io.Copy(w, f)
}

// children lists a job's chunk and audio jobs, for progress displays.
func (s *Server) children(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	kids, err := s.Jobs.Children(r.Context(), id)
	if err != nil {
		s.internalError(w, "get children", err)
		return
	}
	if kids == nil {
		kids = []jobs.Job{}
	}
	writeJSON(w, http.StatusOK, kids)
}
