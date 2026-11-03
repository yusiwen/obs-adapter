// Command obs-adapter serves the OBS service API backed by MinIO.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yusiwen/obs-adapter/internal/config"
	"github.com/yusiwen/obs-adapter/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		slog.Error("obs-adapter exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(log)

	gw, err := gateway.New(cfg, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	authMode := "signature verification disabled (development only)"
	if cfg.ObsAccessKey != "" {
		authMode = "OBS signature verification enabled"
	}
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	log.Info("obs-adapter listening",
		"version", gateway.Version,
		"addr", cfg.Listen,
		"scheme", scheme,
		"domain", cfg.BaseDomain,
		"upstream", cfg.MinioEndpoint,
		"auth", authMode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func parseLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
