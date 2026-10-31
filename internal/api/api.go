// Package api is the HTTP front door: upload a video, read a job's state,
// play a chunked proxy (hls.go).
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"

	"github.com/google/uuid"

	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/storage"
)

const maxIdempotencyKeyLen = 255

type Server struct {
	Jobs      *jobs.Store
	Storage   storage.Storage
	MaxUpload int64 // bytes
	Log       *slog.Logger
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.createJob)
	mux.HandleFunc("GET /jobs/{id}", s.getJob)
	mux.HandleFunc("GET /jobs/{id}/children", s.children)
	s.MountHLS(mux, "/jobs")
	return mux
}

// MountHLS serves a chunked job's playlists and segments under
// prefix/{id}/hls/, so a product API can expose them under its own paths.
func (s *Server) MountHLS(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/{id}/hls/master.m3u8", s.masterPlaylist)
	mux.HandleFunc("GET "+prefix+"/{id}/hls/video.m3u8", s.videoPlaylist)
	mux.HandleFunc("GET "+prefix+"/{id}/hls/audio.m3u8", s.audioPlaylist)
	mux.HandleFunc("GET "+prefix+"/{id}/hls/{file...}", s.segment)
}

// createJob accepts a multipart upload with the video in the "file" field and
// an optional Idempotency-Key header. A retried request with the same key
// returns the original job (200) instead of creating a second one (201).
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var idemKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		if len(k) > maxIdempotencyKeyLen {
			writeError(w, http.StatusBadRequest, "Idempotency-Key too long")
			return
		}
		idemKey = &k
		// Fast path: skip storing the upload again for a known retry.
		// The insert below still handles the race where two arrive at once.
		job, err := s.Jobs.GetByIdempotencyKey(ctx, k)
		if err == nil {
			writeJSON(w, http.StatusOK, job)
			return
		}
		if !errors.Is(err, jobs.ErrNotFound) {
			s.internalError(w, "lookup idempotency key", err)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)
	file, err := filePart(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := uuid.NewString()
	sourceKey := "sources/" + id + ".mp4"
	if _, err := s.Storage.Put(ctx, sourceKey, file); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		s.internalError(w, "store upload", err)
		return
	}

	job, created, err := s.Jobs.Create(ctx, id, sourceKey, idemKey)
	if err != nil || !created {
		// Either the insert failed or a concurrent request with the same key
		// won; our copy of the upload is unreferenced either way.
		if derr := s.Storage.Delete(ctx, sourceKey); derr != nil {
			s.Log.Warn("delete orphaned upload", "key", sourceKey, "err", derr)
		}
	}
	if err != nil {
		s.internalError(w, "create job", err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, job)
		return
	}
	s.Log.Info("job created", "job", job.ID, "idempotency_key", r.Header.Get("Idempotency-Key"))
	writeJSON(w, http.StatusCreated, job)
}

// filePart streams the "file" field without buffering the upload in memory.
func filePart(r *http.Request) (io.Reader, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("expected multipart/form-data with a \"file\" field")
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, errors.New("missing \"file\" field")
		}
		if part.FormName() == "file" {
			return part, nil
		}
		drain(part)
	}
}

func drain(p *multipart.Part) {
	_, _ = io.Copy(io.Discard, p)
	_ = p.Close()
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	job, err := s.Jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		s.internalError(w, "get job", err)
		return
	}
	writeJSON(w, http.StatusOK, job)
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
