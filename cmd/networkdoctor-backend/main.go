// Command networkdoctor-backend receives Alertmanager webhooks and turns them
// into NetworkDoctor incidents.
//
// Detection is not done here. Prometheus rules decide firing/resolved and
// Alertmanager handles grouping, dedup and retry; this service records each
// alert instance exactly once as an incident (idempotent on
// fingerprint+startsAt) and exposes it over HTTP. Evidence collection,
// HolmesGPT RCA and report generation are added in later milestones.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"networkdoctor-agent/internal/backend/api"
	"networkdoctor-agent/internal/backend/config"
	"networkdoctor-agent/internal/backend/incident"
)

func main() {
	cfg := config.ParseFlags()
	logger := log.New(os.Stderr, "networkdoctor-backend: ", log.LstdFlags|log.LUTC)

	store, err := incident.NewFileStore(cfg.DataDir)
	if err != nil {
		logger.Fatalf("open incident store: %v", err)
	}

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: api.New(api.Options{
			Store:        store,
			ClusterName:  cfg.ClusterName,
			MaxBodyBytes: cfg.MaxBodyBytes,
			Logger:       logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s (data-dir=%s cluster=%s prometheus=%q holmes=%q)",
			cfg.Listen, cfg.DataDir, cfg.ClusterName, cfg.PrometheusURL, cfg.HolmesURL)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Fatalf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("serve: %v", err)
		}
	}
}
