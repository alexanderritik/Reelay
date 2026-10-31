// Package chaos is a local control panel for failure injection. It runs real
// worker processes and breaks them on demand (SIGKILL, SIGSTOP/SIGCONT, a
// worker fault mode from CONDUIT_CHAOS), restarts Postgres under them, and shows the job table live,
// so every row of the failure table can be produced and watched in a browser.
// It is a development tool: bind it to localhost only.
package chaos

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/worker"
)

//go:embed ui.html
var page []byte

const logLines = 300 // per worker

type Console struct {
	DB          *pgxpool.Pool
	Jobs        *jobs.Store
	API         http.Handler // mounted at /api
	DataDir     string       // served at /files for playing proxies
	Exe         string       // reelay binary, run as "<Exe> worker"
	WorkerEnv   []string     // added to each worker's environment
	LeaseTTL    time.Duration
	PGContainer string // docker container restarted by /db/restart
	SamplePath  string // generated on first use if missing
	LongSample  string // like SamplePath, but long enough to be chunked
	Log         *slog.Logger

	mu    sync.Mutex
	procs []*proc
	seq   int

	sampleMu sync.Mutex
}

type proc struct {
	Name    string    `json:"name"`
	PID     int       `json:"pid"`
	Fault   string    `json:"fault,omitempty"`
	Status  string    `json:"status"` // running, paused, exited
	Exit    string    `json:"exit,omitempty"`
	Started time.Time `json:"started"`
	Logs    []string  `json:"logs"`
}

func (c *Console) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /state", c.state)
	mux.HandleFunc("GET /sample", c.sample)
	mux.HandleFunc("POST /workers", c.spawn)
	mux.HandleFunc("POST /workers/reset", c.reset)
	mux.HandleFunc("POST /workers/{name}/{action}", c.signal)
	mux.HandleFunc("POST /db/{action}", c.dbControl)
	mux.HandleFunc("POST /jobs/cancel-active", c.cancelActive)
	mux.Handle("/api/", http.StripPrefix("/api", c.API))
	mux.Handle("GET /files/", http.StripPrefix("/files/", http.FileServer(http.Dir(c.DataDir))))
	return mux
}

// Close kills every worker the console started, ffmpeg included.
func (c *Console) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.procs {
		if p.Status != "exited" {
			_ = syscall.Kill(-p.PID, syscall.SIGKILL)
		}
	}
}

func (c *Console) state(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	workers := make([]proc, len(c.procs))
	for i, p := range c.procs {
		workers[i] = *p
		workers[i].Logs = append([]string(nil), p.Logs...)
	}
	c.mu.Unlock()

	out := map[string]any{
		"now":          time.Now(),
		"lease_ttl_ms": c.LeaseTTL.Milliseconds(),
		"workers":      workers,
		"jobs":         []jobs.Job{},
		"children":     map[string][]jobs.Job{}, // chunked roots' chunk and audio jobs
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	list, err := c.Jobs.ListRoots(ctx, 25)
	if err != nil {
		out["db_error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["jobs"] = list
	children := map[string][]jobs.Job{}
	for _, j := range list {
		if j.Chunks == nil {
			continue
		}
		kids, err := c.Jobs.Children(ctx, j.ID)
		if err != nil {
			out["db_error"] = err.Error()
			break
		}
		children[j.ID] = kids
	}
	out["children"] = children
	writeJSON(w, http.StatusOK, out)
}

// spawn starts a worker process in its own process group, so signals reach
// its ffmpeg too: pausing a worker freezes the whole "machine", and killing
// it leaves no orphan transcode behind. Each worker is pinned to one CPU
// core (when taskset exists) so a transcode takes long enough to interrupt.
func (c *Console) spawn(w http.ResponseWriter, r *http.Request) {
	fault := r.URL.Query().Get("fault")
	if !worker.ValidChaos(fault) {
		writeError(w, http.StatusBadRequest, "unknown fault "+strconv.Quote(fault))
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	name := fmt.Sprintf("w%d", c.seq)

	args := []string{c.Exe, "worker"}
	if ts, err := exec.LookPath("taskset"); err == nil {
		core := (c.seq - 1) % runtime.NumCPU()
		args = append([]string{ts, "-c", strconv.Itoa(core)}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(append(os.Environ(), c.WorkerEnv...), "CONDUIT_CHAOS="+fault)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		writeError(w, http.StatusInternalServerError, "start worker: "+err.Error())
		return
	}

	p := &proc{Name: name, PID: cmd.Process.Pid, Fault: fault, Status: "running", Started: time.Now()}
	c.procs = append(c.procs, p)
	c.Log.Info("worker started", "name", name, "pid", p.PID, "fault", fault)

	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			c.mu.Lock()
			p.Logs = append(p.Logs, sc.Text())
			if len(p.Logs) > logLines {
				p.Logs = p.Logs[len(p.Logs)-logLines:]
			}
			c.mu.Unlock()
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
		c.mu.Lock()
		p.Status, p.Exit = "exited", cmd.ProcessState.String()
		c.mu.Unlock()
		c.Log.Info("worker exited", "name", name, "pid", p.PID, "exit", p.Exit)
	}()

	writeJSON(w, http.StatusCreated, p)
}

// signal sends one fault to a worker's process group:
// kill (SIGKILL, a crash), pause (SIGSTOP, a long GC or VM pause),
// resume (SIGCONT) and stop (SIGTERM, a graceful shutdown).
func (c *Console) signal(w http.ResponseWriter, r *http.Request) {
	sigs := map[string]syscall.Signal{
		"kill": syscall.SIGKILL, "pause": syscall.SIGSTOP,
		"resume": syscall.SIGCONT, "stop": syscall.SIGTERM,
	}
	sig, ok := sigs[r.PathValue("action")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	var p *proc
	for _, q := range c.procs {
		if q.Name == r.PathValue("name") {
			p = q
		}
	}
	if p == nil || p.Status == "exited" {
		writeError(w, http.StatusNotFound, "no running worker "+r.PathValue("name"))
		return
	}
	if err := syscall.Kill(-p.PID, sig); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch sig {
	case syscall.SIGSTOP:
		p.Status = "paused"
	case syscall.SIGCONT:
		p.Status = "running"
	case syscall.SIGTERM:
		_ = syscall.Kill(-p.PID, syscall.SIGCONT) // a paused worker must run to shut down
	}
	c.Log.Info("signalled worker", "name", p.Name, "signal", sig)
	writeJSON(w, http.StatusOK, p)
}

// reset kills every worker and forgets the exited ones, for a clean scenario.
func (c *Console) reset(w http.ResponseWriter, _ *http.Request) {
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		alive := 0
		for _, p := range c.procs {
			if p.Status != "exited" {
				alive++
			}
		}
		if alive == 0 {
			c.procs = nil
			c.mu.Unlock()
			writeJSON(w, http.StatusOK, struct{}{})
			return
		}
		c.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	writeError(w, http.StatusInternalServerError, "workers did not exit")
}

// dbControl restarts, stops or starts the Postgres container.
func (c *Console) dbControl(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "restart" && action != "stop" && action != "start" {
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "docker", action, c.PGContainer).CombinedOutput()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("docker %s: %v: %s", action, err, out))
		return
	}
	c.Log.Info("postgres "+action, "container", c.PGContainer, "took", time.Since(start))
	writeJSON(w, http.StatusOK, map[string]any{"took_ms": time.Since(start).Milliseconds()})
}

// cancelActive ends every unfinished job so the next scenario starts from an
// empty queue. A worker still holding one of them is rejected at its next
// fenced write, exactly as if it had lost its lease.
func (c *Console) cancelActive(w http.ResponseWriter, r *http.Request) {
	tag, err := c.DB.Exec(r.Context(), `
		UPDATE jobs
		SET state = 'failed', last_error = 'cancelled by chaos console',
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE state IN ('queued', 'running', 'validating', 'waiting')`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"cancelled": tag.RowsAffected()})
}

// sample serves a test video, generating it on first use: 1080p with a
// running counter and a tone. The default is 60 s, long enough to interrupt
// mid-transcode; ?long=1 serves 120 s, long enough to be chunked.
func (c *Console) sample(w http.ResponseWriter, r *http.Request) {
	path, seconds := c.SamplePath, 60
	if r.URL.Query().Get("long") != "" {
		path, seconds = c.LongSample, 120
	}
	c.sampleMu.Lock()
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		c.Log.Info("generating sample video", "path", path, "seconds", seconds)
		err = generateSample(r.Context(), path, seconds)
	}
	c.sampleMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sample: "+err.Error())
		return
	}
	http.ServeFile(w, r, path)
}

func generateSample(ctx context.Context, path string, seconds int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp.mp4"
	out, err := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(seconds), "-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", tmp).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %v: %s", err, out)
	}
	return os.Rename(tmp, path)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
