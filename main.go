package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alexanderritik/reelay/internal/api"
	"github.com/alexanderritik/reelay/internal/chaos"
	"github.com/alexanderritik/reelay/internal/db"
	"github.com/alexanderritik/reelay/internal/jobs"
	"github.com/alexanderritik/reelay/internal/media"
	"github.com/alexanderritik/reelay/internal/storage"
	"github.com/alexanderritik/reelay/internal/videos"
	"github.com/alexanderritik/reelay/internal/web"
	"github.com/alexanderritik/reelay/internal/worker"
)

const usage = "usage: reelay serve|worker|chaos"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log)
	case "worker":
		err = runWorker(ctx, log)
	case "chaos":
		err = runChaos(ctx, log)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

const defaultDatabaseURL = "postgres://reelay:reelay@localhost:5435/reelay"

func openDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := db.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func runWorker(ctx context.Context, log *slog.Logger) error {
	pool, err := openDB(ctx, env("DATABASE_URL", defaultDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()

	store, err := storage.NewLocal(env("DATA_DIR", "data"))
	if err != nil {
		return err
	}
	workDir := env("WORK_DIR", filepath.Join(os.TempDir(), "reelay-work"))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("WORK_DIR: %w", err)
	}

	var cfgErr error
	w := &worker.Worker{
		ID:           worker.NewID(),
		Jobs:         jobs.NewStore(pool),
		LeaseTTL:     durationEnv("LEASE_TTL", "30s", &cfgErr),
		PollInterval: durationEnv("POLL_INTERVAL", "1s", &cfgErr),
		ReapInterval: durationEnv("REAP_INTERVAL", "5s", &cfgErr),
		Storage:      store,
		WorkDir:      workDir,
		Proxy:        media.DefaultProxy,
		Chaos:        os.Getenv("CONDUIT_CHAOS"),
		Log:          log,
		// Sources this long or longer are split into chunks (worker/chunked.go).
		// The product chunks everything (0), so every video plays as HLS; a
		// higher value sends shorter sources down the single-file MP4 path,
		// which the chaos suite still exercises.
		ChunkSeconds:     durationEnv("CHUNK_SECONDS", "10s", &cfgErr),
		ChunkMinDuration: durationEnvAllowZero("CHUNK_MIN_DURATION", "0s", &cfgErr),
	}
	if cfgErr != nil {
		return cfgErr
	}
	if w.ChunkSeconds%w.Proxy.KeyframeInterval != 0 {
		return fmt.Errorf("CHUNK_SECONDS: %v is not a multiple of the %v keyframe interval",
			w.ChunkSeconds, w.Proxy.KeyframeInterval)
	}
	if !worker.ValidChaos(w.Chaos) {
		return fmt.Errorf("CONDUIT_CHAOS: unknown mode %q", w.Chaos)
	}
	if w.Chaos != "" {
		log.Warn("fault injection enabled", "mode", w.Chaos)
	}
	return w.Run(ctx)
}

// durationEnv parses a duration setting that must be positive, recording the
// first bad value in *errp.
func durationEnv(key, fallback string, errp *error) time.Duration {
	return parseDurationEnv(key, fallback, false, errp)
}

// durationEnvAllowZero is durationEnv for settings where 0 is meaningful.
func durationEnvAllowZero(key, fallback string, errp *error) time.Duration {
	return parseDurationEnv(key, fallback, true, errp)
}

func parseDurationEnv(key, fallback string, allowZero bool, errp *error) time.Duration {
	d, err := time.ParseDuration(env(key, fallback))
	switch {
	case err != nil:
	case d < 0:
		err = errors.New("must not be negative")
	case d == 0 && !allowZero:
		err = errors.New("must be positive")
	}
	if err != nil && *errp == nil {
		*errp = fmt.Errorf("%s: %w", key, err)
	}
	return d
}

func serve(ctx context.Context, log *slog.Logger) error {
	pool, err := openDB(ctx, env("DATABASE_URL", defaultDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := videos.Migrate(ctx, pool); err != nil {
		return err
	}
	store, err := storage.NewLocal(env("DATA_DIR", "data"))
	if err != nil {
		return err
	}
	workDir := env("WORK_DIR", filepath.Join(os.TempDir(), "reelay-work"))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("WORK_DIR: %w", err)
	}

	const maxUpload = 10 << 30 // 10 GiB
	jobStore := jobs.NewStore(pool)
	engine := &api.Server{Jobs: jobStore, Storage: store, MaxUpload: maxUpload, Log: log}

	// The engine's /jobs API stays mounted for debugging and the chaos tools;
	// /videos is the product API and the dashboard is its first client.
	mux := http.NewServeMux()
	mux.Handle("/jobs", engine.Routes())
	mux.Handle("/jobs/", engine.Routes())
	(&videos.Server{
		Videos:    videos.NewStore(pool),
		Jobs:      jobStore,
		Storage:   store,
		Engine:    engine,
		WorkDir:   workDir,
		MaxUpload: maxUpload,
		PublicURL: os.Getenv("PUBLIC_URL"),
		Log:       log,
	}).Routes(mux)
	web.Routes(mux)

	srv := &http.Server{
		Addr:              env("ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", srv.Addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("api stopped")
	return nil
}

// runChaos serves the failure-injection console on localhost. It and the
// workers it starts use their own database and data directory, so clearing
// jobs between scenarios never touches the real queue. Workers get a short
// lease and fast reaping so each scenario runs in seconds.
func runChaos(ctx context.Context, log *slog.Logger) error {
	dbURL, err := db.Sibling(ctx, env("DATABASE_URL", defaultDatabaseURL), env("CHAOS_DB", "reelay_console"))
	if err != nil {
		return err
	}
	pool, err := openDB(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	dataDir := env("CHAOS_DATA_DIR", filepath.Join("data", "console"))
	store, err := storage.NewLocal(dataDir)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	var cfgErr error
	lease := durationEnv("LEASE_TTL", "10s", &cfgErr)
	reap := durationEnv("REAP_INTERVAL", "2s", &cfgErr)
	if cfgErr != nil {
		return cfgErr
	}

	jobStore := jobs.NewStore(pool)
	console := &chaos.Console{
		DB:   pool,
		Jobs: jobStore,
		API: (&api.Server{
			Jobs:      jobStore,
			Storage:   store,
			MaxUpload: 1 << 30,
			Log:       log,
		}).Routes(),
		DataDir: dataDir,
		Exe:     exe,
		WorkerEnv: []string{
			"DATABASE_URL=" + dbURL,
			"DATA_DIR=" + dataDir,
			"LEASE_TTL=" + lease.String(),
			"REAP_INTERVAL=" + reap.String(),
			"POLL_INTERVAL=250ms",
			// The 120 s long sample is chunked; the 60 s sample is not, so the
			// single-file scenarios are unchanged.
			"CHUNK_SECONDS=10s",
			"CHUNK_MIN_DURATION=90s",
		},
		LeaseTTL:    lease,
		PGContainer: env("PG_CONTAINER", "reelay-postgres"),
		SamplePath:  env("CHAOS_SAMPLE", filepath.Join(dataDir, "sample.mp4")),
		LongSample:  env("CHAOS_LONG_SAMPLE", filepath.Join(dataDir, "sample-long.mp4")),
		Log:         log,
	}
	defer console.Close()

	srv := &http.Server{
		Addr:              env("CHAOS_ADDR", "127.0.0.1:8090"),
		Handler:           console.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("chaos console listening", "url", "http://"+srv.Addr, "lease", lease)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
