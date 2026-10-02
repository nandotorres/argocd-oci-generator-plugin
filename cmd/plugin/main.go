// Command plugin runs the ArgoCD OCI ApplicationSet generator plugin server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/auth"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/config"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/generator"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/metrics"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/registry"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/server"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath string
		logLevel   string
		logFormat  string
	)
	flag.StringVar(&configPath, "config", envOr("CONFIG_PATH", "/etc/oci-generator/config.yaml"), "path to config file")
	flag.StringVar(&logLevel, "log-level", envOr("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.StringVar(&logFormat, "log-format", envOr("LOG_FORMAT", "json"), "log format: json|text")
	flag.Parse()

	log := newLogger(logLevel, logFormat)
	slog.SetDefault(log)
	log.Info("starting", slog.String("version", version), slog.String("config", configPath))

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	mx := metrics.New()
	resolver := auth.NewResolver(cfg)
	regClient := registry.New(resolver, registry.Options{
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
		PlainHTTP:          cfg.TLS.PlainHTTP,
		Metrics:            mx,
	})
	gen := generator.New(regClient, log)
	srv := server.New(cfg, gen, log, server.WithMetrics(mx))

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", cfg.Listen))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
