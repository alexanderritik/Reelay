// Command chaos breaks a running Conduit on purpose and checks it recovers.
//
// Workers run as real OS processes against a real Postgres and real ffmpeg.
// Each scenario injects one failure (kill -9, SIGSTOP, a database restart,
// corrupted output, duplicate dispatch, duplicate upload), waits for the
// outcome, and checks it. A final pass checks the invariants across every
// job. Results print as a markdown table for the README.
//
// Run from the repo root with Postgres up (docker-compose up -d):
//
//	go run ./cmd/chaos
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/db"
	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
)

const (
	chaosDB  = "reelay_chaos"
	apiAddr  = "127.0.0.1:8091"
	leaseTTL = "4s" // short, so expiry-driven recovery takes seconds
)

type scenario struct {
	name     string
	expected string
	run      func(context.Context, *harness) (evidence string, err error)
}

var scenarios = []scenario{
	{"Kill worker mid-transcode (`kill -9`)",
		"Lease expires, another worker reclaims, job ends done; no orphaned ffmpeg", killMidTranscode},
	{"Freeze worker past its lease (`SIGSTOP`), then resume",
		"Its late commit is rejected by the fence", freezePastLease},
	{"Worker produces a truncated file",
		"Validation catches it; job retried, never done with a bad file", brokenOutput},
	{"Same job dispatched twice",
		"Only one result committed", doubleDispatch},
	{"Same upload submitted 5× at once (client retries)",
		"One job, one source file", duplicateUpload},
	{"Database restart mid-run",
		"Jobs resume from stored state and finish", databaseRestart},
	{"Job fails every attempt",
		"Ends failed after max retries, with the reason", failsEveryAttempt},

	// Chunked proxies (chunked.go).
	{"Chunked: kill worker mid-chunk (`kill -9`)",
		"Only that chunk is redone; other chunks keep their first result; seams valid, job done", killMidChunk},
	{"Chunked: freeze a chunk worker past its lease, then resume",
		"Its late commit is rejected and the fan-in counts the chunk once", freezeChildPastLease},
	{"Chunked: one child fails every attempt",
		"Root and every unfinished sibling end failed, citing the child", childFailsEveryAttempt},
	{"Chunked: database restart during fan-out",
		"Committed chunks are kept; the rest finish; job done", databaseRestartFanOut},
	{"Chunked: kill worker while planning chunks",
		"No children are left behind; the next owner plans exactly once", crashDuringPlan},
	{"Chunked: kill worker while checking the seams",
		"Only the final check reruns; no chunk is redone", crashDuringFinalize},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chaos:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	h, err := setup(ctx)
	if err != nil {
		return err
	}
	defer h.stopAll()

	type row struct {
		name, expected, result string
		pass                   bool
	}
	var rows []row
	for i, sc := range scenarios {
		fmt.Printf("\n▶ %d. %s\n", i+1, sc.name)
		start := time.Now()
		evidence, err := sc.run(ctx, h)
		h.stopAll()
		r := row{name: sc.name, expected: sc.expected, pass: err == nil}
		if err != nil {
			r.result = "❌ FAIL: " + err.Error()
		} else {
			r.result = "✅ " + evidence
		}
		fmt.Printf("  %s (%s)\n", r.result, time.Since(start).Round(100*time.Millisecond))
		rows = append(rows, r)
	}

	fmt.Printf("\n▶ Invariants across every job\n")
	evidence, err := checkInvariants(ctx, h)
	inv := row{name: "Invariants across all jobs above",
		expected: "Nothing left active; every done job's proxy exists and passes validation; no stray files",
		pass:     err == nil}
	if err != nil {
		inv.result = "❌ FAIL: " + err.Error()
	} else {
		inv.result = "✅ " + evidence
	}
	fmt.Printf("  %s\n", inv.result)
	rows = append(rows, inv)

	fmt.Printf("\n## Failure tests\n\n| # | Failure injected | Expected outcome | Result |\n|---|---|---|---|\n")
	failed := 0
	for i, r := range rows {
		fmt.Printf("| %d | %s | %s | %s |\n", i+1, r.name, r.expected, r.result)
		if !r.pass {
			failed++
		}
	}

	if err := h.printJobs(ctx); err != nil {
		return err
	}
	fmt.Printf("\nLogs and files: %s\n", h.dir)
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(rows))
	}
	return nil
}

// ---------------------------------------------------------------- scenarios

func killMidTranscode(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "long")
	if err != nil {
		return "", err
	}
	a := h.startWorker("1-A")
	if err := a.waitLog("transcoding", 20*time.Second); err != nil {
		return "", err
	}
	time.Sleep(2 * time.Second) // well into the transcode
	a.kill()
	time.Sleep(500 * time.Millisecond)
	orphans := ffmpegPIDs(id)

	b := h.startWorker("1-B")
	job, err := h.waitState(ctx, id, jobs.StateDone, 90*time.Second)
	if err != nil {
		return "", err
	}
	if job.Fence != 2 {
		return "", fmt.Errorf("done at fence %d, want 2 (one reclaim)", job.Fence)
	}
	if !b.hasLog("reaped expired lease") && !h.anyLog("reaped expired lease") {
		return "", errors.New("job finished without being reaped")
	}
	if len(orphans) > 0 {
		for _, pid := range orphans {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return "", fmt.Errorf("job recovered (done at fence %d), but the killed worker left %d ffmpeg "+
			"process(es) running with no parent (pid %v), still burning CPU on a job it no longer owns",
			job.Fence, len(orphans), orphans)
	}
	return fmt.Sprintf("A killed 2s into the transcode; B reaped the lease and finished: done at fence %d, "+
		"no orphaned ffmpeg", job.Fence), nil
}

func freezePastLease(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "short")
	if err != nil {
		return "", err
	}
	a := h.startWorker("2-A", "CONDUIT_CHAOS=freeze-before-commit")
	if err := a.waitLog("chaos: freezing", 30*time.Second); err != nil {
		return "", err
	}
	if err := waitFor(5*time.Second, func() bool { return procState(a.pid()) == 'T' }); err != nil {
		return "", errors.New("worker A did not freeze")
	}
	frozeAt := time.Now()

	b := h.startWorker("2-B")
	job, err := h.waitState(ctx, id, jobs.StateDone, 30*time.Second)
	if err != nil {
		return "", err
	}
	b.stop()
	if job.Fence != 2 {
		return "", fmt.Errorf("B committed at fence %d, want 2", job.Fence)
	}

	frozeFor := time.Since(frozeAt)
	a.signal(syscall.SIGCONT)
	if err := a.waitLog("commit rejected", 10*time.Second); err != nil {
		return "", fmt.Errorf("A resumed but its stale commit was not rejected: %w", err)
	}
	job, err = h.jobs.Get(ctx, id)
	if err != nil {
		return "", err
	}
	want := fmt.Sprintf("proxies/%s/2.mp4", id)
	if job.OutputKey == nil || *job.OutputKey != want || job.Fence != 2 {
		return "", fmt.Errorf("after A's late commit: fence %d output %v, want fence 2 and %s",
			job.Fence, job.OutputKey, want)
	}
	stale := h.dataPath(fmt.Sprintf("proxies/%s/1.mp4", id))
	if err := waitFor(3*time.Second, func() bool { return !exists(stale) }); err != nil {
		return "", errors.New("A's rejected proxy (fence 1) was not deleted")
	}
	return fmt.Sprintf("A froze holding fence 1 for %s; B reclaimed and committed fence 2; "+
		"on resume A's commit matched 0 rows and its proxy was deleted", frozeFor.Round(100*time.Millisecond)), nil
}

func brokenOutput(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "short")
	if err != nil {
		return "", err
	}
	a := h.startWorker("3-A", "CONDUIT_CHAOS=truncate-proxy")
	if err := a.waitLog("attempt failed", 30*time.Second); err != nil {
		return "", err
	}
	a.stop()
	job, err := h.jobs.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if job.State == jobs.StateDone {
		return "", errors.New("truncated proxy was committed")
	}
	if job.LastError == nil || !strings.Contains(*job.LastError, "invalid proxy") {
		return "", fmt.Errorf("last_error = %v, want a validation failure", job.LastError)
	}
	rejected := firstProblem(*job.LastError)

	h.startWorker("3-B")
	job, err = h.waitState(ctx, id, jobs.StateDone, 30*time.Second)
	if err != nil {
		return "", err
	}
	if err := h.validateOutput(ctx, job); err != nil {
		return "", fmt.Errorf("committed proxy fails validation: %w", err)
	}
	return fmt.Sprintf("attempt 1 rejected (%s); attempt 2 done, committed proxy re-validated", rejected), nil
}

func doubleDispatch(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "long")
	if err != nil {
		return "", err
	}
	a := h.startWorker("4-A")
	if err := a.waitLog("transcoding", 20*time.Second); err != nil {
		return "", err
	}
	time.Sleep(time.Second)
	// A buggy dispatcher or an operator re-queues a job that is still running.
	if _, err := h.pool.Exec(ctx, `UPDATE jobs SET state = 'queued', lease_owner = NULL,
		lease_expires_at = NULL, available_at = now() WHERE id = $1`, id); err != nil {
		return "", err
	}
	b := h.startWorker("4-B")
	if err := b.waitLog("transcoding", 10*time.Second); err != nil {
		return "", err
	}
	concurrent := len(ffmpegPIDs(id))
	if err := a.waitLog("stopped work on job", 10*time.Second); err != nil {
		return "", fmt.Errorf("A kept working after losing the job: %w", err)
	}
	job, err := h.waitState(ctx, id, jobs.StateDone, 90*time.Second)
	if err != nil {
		return "", err
	}
	proxies, _ := filepath.Glob(h.dataPath("proxies/" + id + "/*"))
	if len(proxies) != 1 || filepath.Base(proxies[0]) != fmt.Sprintf("%d.mp4", job.Fence) {
		return "", fmt.Errorf("proxy files %v, want exactly one for fence %d", proxies, job.Fence)
	}
	return fmt.Sprintf("%d ffmpeg ran for the job at once; A's next heartbeat failed and it stopped; "+
		"exactly one proxy committed (fence %d)", concurrent, job.Fence), nil
}

func duplicateUpload(ctx context.Context, h *harness) (string, error) {
	api := h.start("5-api", "serve", "ADDR="+apiAddr)
	if err := api.waitLog("api listening", 10*time.Second); err != nil {
		return "", err
	}
	video, err := os.ReadFile(h.sources["short"])
	if err != nil {
		return "", err
	}
	before, _ := filepath.Glob(h.dataPath("sources/*"))

	key := "chaos-" + uuid.NewString()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
		ids   = map[string]bool{}
	)
	for range 5 {
		wg.Go(func() {
			code, id, err := upload(ctx, video, key)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				codes[0]++
				return
			}
			codes[code]++
			ids[id] = true
		})
	}
	wg.Wait()
	after, _ := filepath.Glob(h.dataPath("sources/*"))

	if codes[http.StatusCreated] != 1 || codes[http.StatusOK] != 4 {
		return "", fmt.Errorf("status codes %v, want one 201 and four 200", codes)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("callers saw %d different jobs, want 1", len(ids))
	}
	if n := len(after) - len(before); n != 1 {
		return "", fmt.Errorf("%d source files stored, want 1 (duplicates not cleaned up)", n)
	}
	var id string
	for id = range ids {
	}
	h.startWorker("5-W")
	if _, err := h.waitState(ctx, id, jobs.StateDone, 30*time.Second); err != nil {
		return "", err
	}
	return "1×201 + 4×200, all naming the same job; 1 source file kept; processed once to done", nil
}

func databaseRestart(ctx context.Context, h *harness) (string, error) {
	var ids []string
	for _, src := range []string{"long", "long", "short"} {
		id, err := h.newJob(ctx, src)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	w1, w2 := h.startWorker("6-W1"), h.startWorker("6-W2")
	if err := waitFor(20*time.Second, func() bool {
		return w1.hasLog("transcoding") && w2.hasLog("transcoding")
	}); err != nil {
		return "", errors.New("workers never started transcoding")
	}
	time.Sleep(time.Second)

	start := time.Now()
	if out, err := exec.Command("docker", "restart", h.pgContainer).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker restart: %v: %s", err, out)
	}
	down := time.Since(start)

	retried := 0
	for _, id := range ids {
		job, err := h.waitState(ctx, id, jobs.StateDone, 120*time.Second)
		if err != nil {
			return "", err
		}
		if job.Fence > 1 {
			retried++
		}
	}
	dbErrors := w1.countLog("failed") + w2.countLog("failed")
	return fmt.Sprintf("Postgres restarted mid-transcode (%s); workers logged %d DB errors and kept going; "+
		"all 3 jobs done (%d needed a retry)", down.Round(100*time.Millisecond), dbErrors, retried), nil
}

func failsEveryAttempt(ctx context.Context, h *harness) (string, error) {
	id, err := h.newJob(ctx, "short")
	if err != nil {
		return "", err
	}
	a := h.startWorker("7-A", "CONDUIT_CHAOS=truncate-proxy")
	job, err := h.waitState(ctx, id, jobs.StateFailed, 90*time.Second)
	if err != nil {
		return "", err
	}
	a.stop()
	if job.Attempts != job.MaxAttempts {
		return "", fmt.Errorf("failed after %d attempts, want %d", job.Attempts, job.MaxAttempts)
	}
	if job.LastError == nil || !strings.HasPrefix(*job.LastError, fmt.Sprintf("attempt %d of %d: invalid proxy",
		job.MaxAttempts, job.MaxAttempts)) {
		return "", fmt.Errorf("last_error = %v, want the final validation failure", job.LastError)
	}
	claims := a.logTimes("claimed job")
	var gaps []string
	for i := 1; i < len(claims); i++ {
		gaps = append(gaps, claims[i].Sub(claims[i-1]).Round(100*time.Millisecond).String())
	}
	return fmt.Sprintf("%d attempts, each rejected by validation, claims %s apart (jittered backoff); "+
		"ended failed: %q", job.Attempts, strings.Join(gaps, " and "), shorten(*job.LastError, 70)), nil
}

// checkInvariants runs after every scenario: the end state must be clean no
// matter what was done to the system.
func checkInvariants(ctx context.Context, h *harness) (string, error) {
	all, err := h.allJobs(ctx)
	if err != nil {
		return "", err
	}
	var problems []string
	committed := map[string]bool{} // single-file keys, and child prefixes ending in "/"
	done := 0
	for _, j := range all {
		switch j.State {
		case jobs.StateDone:
			done++
			switch {
			case j.Kind != jobs.KindProxy:
				committed[*j.OutputKey+"/"] = true
			case j.Chunks == nil:
				committed[*j.OutputKey] = true
			}
			if err := h.validateOutput(ctx, j); err != nil {
				problems = append(problems, fmt.Sprintf("job %s: %v", j.ID[:8], err))
			}
		case jobs.StateFailed:
		default:
			problems = append(problems, fmt.Sprintf("job %s left %s", j.ID[:8], j.State))
		}
	}
	for _, rel := range h.proxyFiles() {
		if !committedFile(rel, committed) {
			problems = append(problems, "stray proxy "+rel)
		}
	}
	leftover, _ := os.ReadDir(h.workDir)
	for _, e := range leftover {
		problems = append(problems, "leftover work dir "+e.Name())
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	return fmt.Sprintf("%d jobs (chunks included): %d done (every output re-validated), %d failed; 0 active, "+
		"0 stray proxies, 0 leftover work dirs", len(all), done, len(all)-done), nil
}

// ------------------------------------------------------------------ harness

type harness struct {
	dir, bin, workDir string
	dbURL             string
	pgContainer       string
	pool              *pgxpool.Pool
	jobs              *jobs.Store
	sources           map[string]string // name -> path on disk
	srcOf             map[string]string // job ID -> source path

	mu    sync.Mutex
	procs []*proc
}

func setup(ctx context.Context) (*harness, error) {
	if _, err := os.Stat("go.mod"); err != nil {
		return nil, errors.New("run from the repo root")
	}
	dir, err := os.MkdirTemp("", "reelay-chaos-")
	if err != nil {
		return nil, err
	}
	h := &harness{
		dir:         dir,
		bin:         filepath.Join(dir, "reelay"),
		workDir:     filepath.Join(dir, "work"),
		pgContainer: env("CHAOS_PG_CONTAINER", "reelay-postgres"),
		sources:     map[string]string{},
		srcOf:       map[string]string{},
	}
	for _, d := range []string{h.workDir, h.dataPath("sources"), filepath.Join(dir, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	fmt.Println("building reelay...")
	if out, err := exec.Command("go", "build", "-o", h.bin, ".").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build: %v: %s", err, out)
	}

	adminURL := env("DATABASE_URL", "postgres://reelay:reelay@localhost:5435/reelay")
	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("postgres (docker-compose up -d?): %w", err)
	}
	var exists bool
	_ = admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, chaosDB).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+chaosDB); err != nil {
			return nil, err
		}
	}
	c := admin.Config().ConnConfig
	admin.Close()
	h.dbURL = fmt.Sprintf("postgres://%s:%s@%s:%d/%s", c.User, c.Password, c.Host, c.Port, chaosDB)
	if h.pool, err = db.Connect(ctx, h.dbURL); err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, h.pool); err != nil {
		return nil, err
	}
	if _, err := h.pool.Exec(ctx, `TRUNCATE jobs`); err != nil {
		return nil, err
	}
	h.jobs = jobs.NewStore(h.pool)

	fmt.Println("rendering test videos...")
	for name, spec := range map[string][2]string{
		"short":   {"1280x720", "4"},   // ~0.5s to transcode
		"long":    {"3840x2160", "90"}, // ~10s to transcode: room to strike mid-way
		"chunked": {"3840x2160", "40"}, // 10 chunks of 4s for chunk workers; single-file for the rest
	} {
		path := filepath.Join(dir, name+".mp4")
		out, err := exec.Command("ffmpeg", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size="+spec[0]+":rate=30", "-f", "lavfi", "-i", "sine",
			"-t", spec[1], "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("render %s: %v: %s", name, err, out)
		}
		h.sources[name] = path
	}
	fmt.Println("artifacts:", dir)
	return h, nil
}

func (h *harness) dataPath(key string) string {
	return filepath.Join(h.dir, "data", filepath.FromSlash(key))
}

// newJob stores a copy of a test video as a new job's source and queues it.
func (h *harness) newJob(ctx context.Context, source string) (string, error) {
	id := uuid.NewString()
	key := "sources/" + id + ".mp4"
	if err := os.Link(h.sources[source], h.dataPath(key)); err != nil {
		return "", err
	}
	if _, _, err := h.jobs.Create(ctx, id, key, nil); err != nil {
		return "", err
	}
	h.srcOf[id] = h.sources[source]
	return id, nil
}

// waitState polls until the job reaches state. Database errors are retried:
// the database-restart scenario takes Postgres away underneath us too.
func (h *harness) waitState(ctx context.Context, id string, state jobs.State, timeout time.Duration) (jobs.Job, error) {
	deadline := time.Now().Add(timeout)
	var last jobs.Job
	for time.Now().Before(deadline) {
		if j, err := h.jobs.Get(ctx, id); err == nil {
			last = j
			if j.State == state {
				return j, nil
			}
			if state == jobs.StateDone && j.State == jobs.StateFailed {
				return j, fmt.Errorf("job failed: %s", deref(j.LastError))
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last, fmt.Errorf("job not %s after %s (state %s, fence %d)", state, timeout, last.State, last.Fence)
}

func (h *harness) validateOutput(ctx context.Context, j jobs.Job) error {
	if j.OutputKey == nil {
		return errors.New("done without output")
	}
	src := h.srcOf[j.ID]
	if src == "" {
		src = h.dataPath(j.SourceKey)
	}
	if j.Kind != jobs.KindProxy || j.Chunks != nil {
		return h.validateGraphOutput(ctx, j, src)
	}
	path := h.dataPath(*j.OutputKey)
	if !exists(path) {
		return fmt.Errorf("output %s missing", *j.OutputKey)
	}
	return media.Validate(ctx, src, path, media.DefaultProxy)
}

func (h *harness) allJobs(ctx context.Context) ([]jobs.Job, error) {
	rows, err := h.pool.Query(ctx, `SELECT id FROM jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var all []jobs.Job
	for _, id := range ids {
		j, err := h.jobs.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		all = append(all, j)
	}
	return all, rows.Err()
}

func (h *harness) printJobs(ctx context.Context) error {
	all, err := h.allJobs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n## Final state of every job\n\n| Job | State | Fence | Attempts | Output | Last error |\n|---|---|---|---|---|---|\n")
	for _, j := range all {
		out := "-"
		if j.OutputKey != nil {
			out = strings.TrimPrefix(*j.OutputKey, "proxies/"+j.ID+"/")
		}
		fmt.Printf("| %s | %s | %d | %d/%d | %s | %s |\n", j.ID[:8], j.State, j.Fence,
			j.Attempts, j.MaxAttempts, out, shorten(deref(j.LastError), 90))
	}
	return nil
}

// ---------------------------------------------------------------- processes

type proc struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}

	mu      sync.Mutex
	lines   []string
	partial []byte
	logFile *os.File
}

func (h *harness) startWorker(name string, env ...string) *proc {
	return h.start(name, "worker", env...)
}

func (h *harness) start(name, command string, env ...string) *proc {
	cmd := exec.Command(h.bin, command)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+h.dbURL,
		"DATA_DIR="+h.dataPath(""),
		"WORK_DIR="+h.workDir,
		"LEASE_TTL="+leaseTTL,
		"REAP_INTERVAL=1s",
		"POLL_INTERVAL=200ms",
	)
	cmd.Env = append(cmd.Env, env...)
	logFile, _ := os.Create(filepath.Join(h.dir, "logs", name+".log"))
	p := &proc{name: name, cmd: cmd, done: make(chan struct{}), logFile: logFile}
	cmd.Stdout, cmd.Stderr = p, p
	if err := cmd.Start(); err != nil {
		panic(fmt.Sprintf("start %s: %v", name, err))
	}
	go func() { _ = cmd.Wait(); close(p.done) }()

	h.mu.Lock()
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	fmt.Printf("  started %s (pid %d) %s\n", name, cmd.Process.Pid, strings.Join(env, " "))
	return p
}

func (p *proc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logFile != nil {
		_, _ = p.logFile.Write(b)
	}
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			break
		}
		p.lines = append(p.lines, string(p.partial[:i]))
		p.partial = p.partial[i+1:]
	}
	return len(b), nil
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *proc) hasLog(substr string) bool { return p.countLog(substr) > 0 }

func (p *proc) countLog(substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, l := range p.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (p *proc) waitLog(substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.hasLog(substr) {
			return nil
		}
		if p.exited() {
			return fmt.Errorf("%s exited before logging %q", p.name, substr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not log %q within %s", p.name, substr, timeout)
}

var timeField = regexp.MustCompile(`^time=(\S+)`)

// logTimes returns the timestamps of log lines containing substr.
func (p *proc) logTimes(substr string) []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ts []time.Time
	for _, l := range p.lines {
		if m := timeField.FindStringSubmatch(l); m != nil && strings.Contains(l, substr) {
			if t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", m[1]); err == nil {
				ts = append(ts, t)
			}
		}
	}
	return ts
}

func (p *proc) signal(sig syscall.Signal) { _ = p.cmd.Process.Signal(sig) }

func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
	fmt.Printf("  kill -9 %s\n", p.name)
}

// stop asks the process to exit (resuming it first if frozen) and kills it
// if it does not.
func (p *proc) stop() {
	if p.exited() {
		return
	}
	p.signal(syscall.SIGCONT)
	p.signal(syscall.SIGINT)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (h *harness) stopAll() {
	h.mu.Lock()
	procs := slices.Clone(h.procs)
	h.procs = nil
	h.mu.Unlock()
	for _, p := range procs {
		p.stop()
		if p.logFile != nil {
			p.logFile.Close()
		}
	}
}

func (h *harness) anyLog(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.ContainsFunc(h.procs, func(p *proc) bool { return p.hasLog(substr) })
}

// ffmpegPIDs finds running ffmpeg processes whose arguments mention substr.
func ffmpegPIDs(substr string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscan(e.Name(), &pid); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if filepath.Base(args[0]) == "ffmpeg" && strings.Contains(string(cmdline), substr) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// procState returns a process's state letter from /proc (T = stopped).
func procState(pid int) byte {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2]
	}
	return 0
}

// ------------------------------------------------------------------ helpers

func upload(ctx context.Context, video []byte, key string) (code int, id string, err error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "video.mp4")
	_, _ = fw.Write(video)
	_ = mw.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+apiAddr+"/jobs", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := regexp.MustCompile(`"id":"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return resp.StatusCode, "", fmt.Errorf("no job id in %s", b)
	}
	return resp.StatusCode, string(m[1]), nil
}

func waitFor(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timed out")
}

// firstProblem pulls the first validation problem out of a last_error.
func firstProblem(lastError string) string {
	_, after, ok := strings.Cut(lastError, "invalid proxy: ")
	if !ok {
		return shorten(lastError, 60)
	}
	first, _, _ := strings.Cut(after, ";")
	return first
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
