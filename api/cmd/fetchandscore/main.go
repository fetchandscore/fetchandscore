// Command fetchandscore serves the Fetch and Score API.
//
// Everything is configured through the environment, so the same image runs on
// a laptop, a home server behind a tunnel, or any VPS without a rebuild.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/httpapi"
	"github.com/fetchandscore/fetchandscore/api/internal/mail"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := loadConfig()
	log := newLogger(cfg.dev)

	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	log.Info("database ready", "path", cfg.dbPath)

	mailer, err := cfg.mailer(log)
	if err != nil {
		return err
	}

	authSvc := auth.New(st, mailer, cfg.baseURL)
	hub := httpapi.NewHub()
	api := httpapi.NewServer(st, authSvc, hub, httpapi.Config{
		AllowedOrigin: cfg.allowedOrigin,
		SecureCookies: cfg.secureCookies,
	}, log)

	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: api.Handler(),
		// Generous relative to a JSON API because the SSE stream is a normal
		// response that stays open; WriteTimeout would cut it off, so it stays
		// zero and the stream's own context governs its lifetime.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go api.StartHousekeeping(ctx, time.Hour)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "origin", cfg.allowedOrigin)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Give in-flight requests a moment. Open SSE streams are cancelled by the
	// server context, so they end promptly rather than holding this open.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

type config struct {
	addr          string
	dbPath        string
	baseURL       string
	allowedOrigin string
	secureCookies bool
	dev           bool

	mailgunDomain  string
	mailgunKey     string
	mailgunBaseURL string
	mailFrom       string
}

func loadConfig() config {
	cfg := config{
		addr:           env("FNS_ADDR", ":8080"),
		dbPath:         env("FNS_DB_PATH", "data/fetchandscore.db"),
		baseURL:        env("FNS_BASE_URL", "https://fetchandscore.com"),
		dev:            envBool("FNS_DEV", false),
		mailgunDomain:  env("FNS_MAILGUN_DOMAIN", ""),
		mailgunKey:     env("FNS_MAILGUN_API_KEY", ""),
		mailgunBaseURL: env("FNS_MAILGUN_BASE_URL", ""),
		mailFrom:       env("FNS_MAIL_FROM", "Fetch and Score <no-reply@fetchandscore.com>"),
	}

	// The frontend is served from the site origin, so that is the only origin
	// allowed to call the API unless told otherwise.
	cfg.allowedOrigin = env("FNS_ALLOWED_ORIGIN", cfg.baseURL)
	cfg.secureCookies = envBool("FNS_SECURE_COOKIES", !cfg.dev)
	return cfg
}

// mailer picks a sender. In development the sign-in link is written to the log
// instead of being emailed, which is the whole point of development mode.
func (c config) mailer(log *slog.Logger) (mail.Sender, error) {
	if c.mailgunDomain != "" && c.mailgunKey != "" {
		return &mail.Mailgun{
			Domain:  c.mailgunDomain,
			APIKey:  c.mailgunKey,
			From:    c.mailFrom,
			BaseURL: c.mailgunBaseURL,
		}, nil
	}

	if !c.dev {
		return nil, errors.New(
			"no mail configured: set FNS_MAILGUN_DOMAIN and FNS_MAILGUN_API_KEY, or FNS_DEV=1 to log links instead")
	}

	log.Warn("development mode: sign-in links are logged, not emailed")
	return &mail.Recorder{OnSend: func(m mail.Message) {
		log.Info("sign-in link", "to", m.To, "body", m.Text)
	}}, nil
}

func newLogger(dev bool) *slog.Logger {
	level := slog.LevelInfo
	if dev {
		level = slog.LevelDebug
	}
	// Structured JSON in production so a log shipper can read it; plain text
	// on a developer's terminal.
	if dev {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// ensureDir is used by the store path so a fresh deployment does not need the
// operator to create the directory by hand.
func init() {
	if path := os.Getenv("FNS_DB_PATH"); path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o750)
	} else {
		_ = os.MkdirAll("data", 0o750)
	}
}
