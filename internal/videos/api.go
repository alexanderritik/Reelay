package videos

// The product API. Uploading goes through the engine's job table exactly
// like /jobs does; everything else here reads it.
//
//	POST   /videos                    multipart "file" (+ optional "title") → video
//	GET    /videos                    newest first
//	GET    /videos/{id}               one video, with its chunk map
//	PATCH  /videos/{id}               {"title": "..."}
//	DELETE /videos/{id}               a ready or failed video
//	GET    /videos/{id}/thumbnail.jpg poster frame, made from the source on first request
//	GET    /videos/{id}/hls/…         HLS playback, playable while processing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/alexanderritik/reelay/internal/api"
	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
	"github.com/alexanderritik/reelay/internal/storage"
)

const (
	maxIdempotencyKeyLen = 255
	maxTitleLen          = 200
	listLimit            = 200
	thumbnailWidth       = 640
)

type Server struct {
	Videos    *Store
	Jobs      *jobs.Store
	Storage   storage.Storage
	Engine    *api.Server // serves the HLS playlists and segments
	WorkDir   string      // scratch space for probing uploads and making thumbnails
	MaxUpload int64       // bytes
	PublicURL string      // base for absolute URLs; empty derives it from the request
	Log       *slog.Logger

	thumbs chan struct{} // bounds concurrent thumbnail ffmpeg runs
}

// Video is the API's view of an upload.
type Video struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Filename     string    `json:"filename,omitempty"`
	Status       Status    `json:"status"`
	Progress     float64   `json:"progress"`
	Playable     bool      `json:"playable"`         // play_url works now, possibly before ready
	Format       string    `json:"format,omitempty"` // "hls", once planned
	PlayURL      string    `json:"play_url,omitempty"`
	ThumbnailURL string    `json:"thumbnail_url"`
	EmbedURL     string    `json:"embed_url"`
	Duration     *float64  `json:"duration,omitempty"` // seconds
	SourceHeight *int      `json:"source_height,omitempty"`
	SizeBytes    int64     `json:"size_bytes"`
	Error        *string   `json:"error,omitempty"`
	Attempts     int       `json:"attempts"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Parts        []Part    `json:"parts,omitempty"` // GET /videos/{id} only
}

// Part is one chunk or audio job of a chunked video, for the chunk map.
type Part struct {
	Kind     jobs.Kind  `json:"kind"`
	Index    *int       `json:"index,omitempty"`
	State    jobs.State `json:"state"`
	Progress float64    `json:"progress"`
	Attempts int        `json:"attempts"`
	Start    *float64   `json:"start,omitempty"`
	End      *float64   `json:"end,omitempty"`
}

func (s *Server) Routes(mux *http.ServeMux) {
	s.thumbs = make(chan struct{}, 2)
	mux.HandleFunc("POST /videos", s.create)
	mux.HandleFunc("GET /videos", s.list)
	mux.HandleFunc("GET /videos/{id}", s.get)
	mux.HandleFunc("PATCH /videos/{id}", s.rename)
	mux.HandleFunc("DELETE /videos/{id}", s.delete)
	mux.HandleFunc("GET /videos/{id}/thumbnail.jpg", s.thumbnail)
	s.Engine.MountHLS(mux, "/videos")
}

func (s *Server) view(r *http.Request, v row) Video {
	base := s.base(r) + "/videos/" + v.ID
	format, playable := v.playback()
	out := Video{
		ID: v.ID, Title: v.Title, Filename: v.Filename,
		Status: v.status(), Progress: math.Round(v.progress()*10) / 10,
		Playable: playable, Format: format,
		ThumbnailURL: base + "/thumbnail.jpg",
		EmbedURL:     s.base(r) + "/embed/" + v.ID,
		Duration:     v.Duration, SourceHeight: v.SourceHeight, SizeBytes: v.SizeBytes,
		Error: v.LastError, Attempts: v.Attempts,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
	if out.Title == "" {
		out.Title = "Untitled video"
	}
	if out.Status != StatusFailed {
		out.Error = nil // last_error of an attempt that was retried and recovered
	}
	if format == "hls" {
		out.PlayURL = base + "/hls/master.m3u8"
	}
	return out
}

// base is the scheme and host clients should use to reach this server.
func (s *Server) base(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimSuffix(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// create stores the upload, checks it is a video ffmpeg can read, and queues
// it. A retried request with the same Idempotency-Key returns the original
// video (200) instead of creating a second one (201).
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var idemKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		if len(k) > maxIdempotencyKeyLen {
			writeError(w, http.StatusBadRequest, "Idempotency-Key too long")
			return
		}
		idemKey = &k
		if job, err := s.Jobs.GetByIdempotencyKey(ctx, k); err == nil {
			s.respond(w, r, http.StatusOK, job.ID)
			return
		} else if !errors.Is(err, jobs.ErrNotFound) {
			s.internalError(w, "lookup idempotency key", err)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, `expected multipart/form-data with a "file" field`)
		return
	}

	id := uuid.NewString()
	sourceKey := "sources/" + id + ".mp4"
	var meta Meta
	stored := false
	cleanup := func() {
		if derr := s.Storage.Delete(context.WithoutCancel(ctx), sourceKey); derr != nil {
			s.Log.Warn("delete unused upload", "key", sourceKey, "err", derr)
		}
	}

	// Read every part: "title" may come before or after "file".
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if stored {
				cleanup()
			}
			writeError(w, http.StatusBadRequest, "malformed multipart body")
			return
		}
		switch part.FormName() {
		case "title":
			b, _ := io.ReadAll(io.LimitReader(part, 4*maxTitleLen))
			meta.Title = cleanTitle(string(b))
		case "file":
			if stored {
				break // only the first file counts
			}
			meta.Filename = filepath.Base(part.FileName())
			n, err := s.Storage.Put(ctx, sourceKey, part)
			if err != nil {
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
					return
				}
				s.internalError(w, "store upload", err)
				return
			}
			meta.SizeBytes, stored = n, true
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
	if !stored {
		writeError(w, http.StatusBadRequest, `missing "file" field`)
		return
	}

	// Reject what can never be processed now, while the client is still
	// here to hear about it, instead of queueing a job that fails later.
	info, err := s.probe(ctx, sourceKey)
	if err != nil {
		cleanup()
		if errors.Is(err, media.ErrBadSource) {
			// ffprobe's message names server paths; it goes to the log only.
			s.Log.Info("upload rejected", "filename", meta.Filename, "err", err)
			writeError(w, http.StatusUnprocessableEntity, "this file is not a video we can read")
			return
		}
		s.internalError(w, "probe upload", err)
		return
	}
	meta.Duration, meta.SourceHeight = info.Duration.Seconds(), info.Height
	if meta.Title == "" {
		meta.Title = cleanTitle(strings.TrimSuffix(meta.Filename, filepath.Ext(meta.Filename)))
	}

	job, created, err := s.Jobs.Create(ctx, id, sourceKey, idemKey)
	if err != nil || !created {
		cleanup() // the insert failed, or a concurrent retry with this key won
	}
	if err != nil {
		s.internalError(w, "create job", err)
		return
	}
	if !created {
		s.respond(w, r, http.StatusOK, job.ID)
		return
	}
	if err := s.Videos.Insert(ctx, id, meta); err != nil {
		// The job is queued and will process; it just shows as untitled.
		s.Log.Error("record video metadata", "video", id, "err", err)
	}
	s.Log.Info("video uploaded", "video", id, "bytes", meta.SizeBytes, "duration", info.Duration.Round(time.Millisecond))
	s.respond(w, r, http.StatusCreated, id)
}

func (s *Server) probe(ctx context.Context, key string) (media.SourceInfo, error) {
	dir, err := os.MkdirTemp(s.WorkDir, "probe-")
	if err != nil {
		return media.SourceInfo{}, err
	}
	defer os.RemoveAll(dir)
	src, err := storage.LocalFile(ctx, s.Storage, key, dir)
	if err != nil {
		return media.SourceInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return media.Probe(ctx, src)
}

func cleanTitle(t string) string {
	t = strings.Join(strings.Fields(strings.ToValidUTF8(t, "")), " ")
	for utf8.RuneCountInString(t) > maxTitleLen {
		_, size := utf8.DecodeLastRuneInString(t)
		t = t[:len(t)-size]
	}
	return t
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, id string) {
	v, err := s.Videos.get(r.Context(), id)
	if err != nil {
		s.internalError(w, "get video", err)
		return
	}
	writeJSON(w, status, s.view(r, v))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	limit := listLimit
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < listLimit {
		limit = n
	}
	rows, err := s.Videos.list(r.Context(), limit)
	if err != nil {
		s.internalError(w, "list videos", err)
		return
	}
	out := make([]Video, len(rows))
	for i, v := range rows {
		out[i] = s.view(r, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	v, err := s.Videos.get(r.Context(), id)
	if s.notFound(w, err) {
		return
	}
	out := s.view(r, v)
	if v.Chunks != nil {
		kids, err := s.Jobs.Children(r.Context(), id)
		if err != nil {
			s.internalError(w, "get children", err)
			return
		}
		for _, k := range kids {
			out.Parts = append(out.Parts, Part{Kind: k.Kind, Index: k.ChunkIndex, State: k.State,
				Progress: k.Progress, Attempts: k.Attempts, Start: k.RangeStart, End: k.RangeEnd})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil || cleanTitle(body.Title) == "" {
		writeError(w, http.StatusBadRequest, `expected {"title": "..."}`)
		return
	}
	if s.notFound(w, s.Videos.Rename(r.Context(), id, cleanTitle(body.Title))) {
		return
	}
	s.respond(w, r, http.StatusOK, id)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	err := s.Videos.Delete(r.Context(), id)
	if errors.Is(err, ErrBusy) {
		writeError(w, http.StatusConflict, "video is still processing; delete it once it is ready or failed")
		return
	}
	if s.notFound(w, err) {
		return
	}
	// The rows are gone, so nothing points at these files any more.
	ctx := context.WithoutCancel(r.Context())
	for _, err := range []error{
		s.Storage.Delete(ctx, "sources/"+id+".mp4"),
		s.Storage.DeletePrefix(ctx, "proxies/"+id),
		s.Storage.Delete(ctx, thumbnailKey(id)),
	} {
		if err != nil {
			s.Log.Warn("delete video files", "video", id, "err", err)
		}
	}
	s.Log.Info("video deleted", "video", id)
	w.WriteHeader(http.StatusNoContent)
}

func thumbnailKey(id string) string { return "thumbnails/" + id + ".jpg" }

// thumbnail serves a poster frame. It is made from the source on first
// request, so it exists as soon as the upload does, long before the video
// is ready.
func (s *Server) thumbnail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	f, err := s.Storage.Open(r.Context(), thumbnailKey(id))
	if err != nil {
		if err := s.makeThumbnail(r.Context(), id); err != nil {
			if !errors.Is(err, jobs.ErrNotFound) {
				s.Log.Warn("make thumbnail", "video", id, "err", err)
			}
			writeError(w, http.StatusNotFound, "no thumbnail")
			return
		}
		if f, err = s.Storage.Open(r.Context(), thumbnailKey(id)); err != nil {
			s.internalError(w, "open thumbnail", err)
			return
		}
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.Copy(w, f)
}

func (s *Server) makeThumbnail(ctx context.Context, id string) error {
	job, err := s.Jobs.Get(ctx, id)
	if err != nil {
		return err
	}
	select {
	case s.thumbs <- struct{}{}:
		defer func() { <-s.thumbs }()
	case <-ctx.Done():
		return ctx.Err()
	}
	dir, err := os.MkdirTemp(s.WorkDir, "thumb-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src, err := storage.LocalFile(ctx, s.Storage, job.SourceKey, dir)
	if err != nil {
		return err
	}
	info, err := media.Probe(ctx, src)
	if err != nil {
		return err
	}
	// A quarter in, at most 2 s: past fades from black, still early.
	at := min(2, info.Duration.Seconds()/4)
	out := filepath.Join(dir, "thumb.jpg")
	if err := media.Thumbnail(ctx, src, out, at, thumbnailWidth); err != nil {
		return err
	}
	f, err := os.Open(out)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = s.Storage.Put(ctx, thumbnailKey(id), f)
	return err
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		writeError(w, http.StatusNotFound, "video not found")
		return "", false
	}
	return id, true
}

// notFound writes the response for a failed lookup and reports whether it did.
func (s *Server) notFound(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "video not found")
	default:
		s.internalError(w, "video", err)
	}
	return true
}

func (s *Server) internalError(w http.ResponseWriter, op string, err error) {
	s.Log.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
