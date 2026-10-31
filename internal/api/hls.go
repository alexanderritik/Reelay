package api

// Playback of chunked proxies. Playlists are not stored: every request
// renders them from the job table, which is the only source of truth, so
// concurrent chunk commits can never race to write a playlist file, and a
// segment appears only after its fenced commit. At high request rates this
// is the part to cache.
//
//	GET /jobs/{id}/hls/master.m3u8   video + audio renditions
//	GET /jobs/{id}/hls/video.m3u8    EVENT playlist, grows as chunks commit
//	GET /jobs/{id}/hls/audio.m3u8    the audio rendition
//	GET /jobs/{id}/hls/{file...}     init and media segments

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

// masterBandwidth is the peak bitrate advertised for the one rendition. A
// 540p CRF 23 proxy plus 128k audio stays well under it; with a single
// rendition a player only uses it to size its buffer.
const masterBandwidth = 3_000_000

// graph is a chunked job as the playlists see it.
type graph struct {
	root   jobs.Job
	chunks []jobs.Job // in index order
	audio  *jobs.Job
}

// playable is the run of committed chunks from chunk 0, stopping at the
// first one not yet done. It only ever grows, because done is final, which
// is exactly what an EVENT playlist allows.
func (g graph) playable() []hls.Part {
	var parts []hls.Part
	for _, c := range g.chunks {
		if c.State != jobs.StateDone {
			break
		}
		parts = append(parts, part(g.root.ID, c))
	}
	return parts
}

// ready reports whether a player can start: audio (if any) and chunk 0 are in.
func (g graph) ready() bool {
	if g.audio != nil && g.audio.State != jobs.StateDone {
		return false
	}
	return len(g.playable()) > 0
}

func part(rootID string, child jobs.Job) hls.Part {
	return hls.Part{
		Dir:   strings.TrimPrefix(*child.OutputKey, "proxies/"+rootID+"/"),
		Media: *child.HLS,
	}
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
	writePlaylist(w, hls.MasterPlaylist(masterBandwidth, g.audio != nil), false)
}

func (s *Server) videoPlaylist(w http.ResponseWriter, r *http.Request) {
	g, ok := s.loadGraph(w, r)
	if !ok {
		return
	}
	parts := g.playable()
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
	writePlaylist(w, hls.AudioPlaylist(part(g.root.ID, *g.audio)), false)
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
