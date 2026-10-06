// Command networkdoctor-backend receives Alertmanager webhooks and turns them
// into NetworkDoctor incidents.
//
// Detection is not done here. Prometheus rules decide firing/resolved and
// Alertmanager handles grouping, dedup and retry; this service records each
// alert instance exactly once as an incident (idempotent on
// fingerprint+startsAt), collects a PromQL evidence snapshot, asks HolmesGPT
// once for a root cause, and serves the result as JSON and Markdown.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"networkdoctor-agent/internal/backend/api"
	"networkdoctor-agent/internal/backend/check"
	"networkdoctor-agent/internal/backend/config"
	"networkdoctor-agent/internal/backend/evidence"
	"networkdoctor-agent/internal/backend/features"
	"networkdoctor-agent/internal/backend/holmes"
	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/investigate"
	"networkdoctor-agent/internal/backend/prometheus"
)

func main() {
	cfg := config.ParseFlags()
	logger := log.New(os.Stderr, "networkdoctor-backend: ", log.LstdFlags|log.LUTC)

	store, err := incident.NewFileStore(cfg.DataDir)
	if err != nil {
		logger.Fatalf("open incident store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	skip := investigate.ParseSkipPrefixes(cfg.HolmesSkipRulePrefixes)
	eligible := func(i *incident.Incident) bool {
		for _, p := range skip {
			if strings.HasPrefix(i.RuleID, p) {
				return false
			}
		}
		return true
	}

	// Evidence + Holmes are optional: each is enabled by its URL.
	arch, err := investigate.ParseArch(cfg.Arch)
	if err != nil {
		logger.Fatalf("ND_ARCH: %v", err)
	}
	var inv api.Investigator
	if cfg.HolmesURL != "" || cfg.PrometheusURL != "" {
		opts := investigate.Options{
			Arch:         arch,
			EvalDir:      cfg.EvalDir,
			Store:        store,
			Model:        cfg.HolmesModel,
			Window:       cfg.EvidenceWindow,
			Timeout:      cfg.HolmesTimeout,
			SkipPrefixes: skip,
			Workers:      cfg.Workers,
			GroupWait:    cfg.GroupWait,
			Logger:       logger,
		}
		if cfg.PrometheusURL != "" {
			prom := prometheus.New(cfg.PrometheusURL)
			opts.Evidence = &evidence.Collector{Prom: prom}
			opts.Deriver = &features.Deriver{Prom: prom}
		}
		// A typed nil *check.Jev must not become a non-nil interface.
		if j := check.NewJev(cfg.JevURL, cfg.JevModel, cfg.JevAPIKey); j != nil {
			opts.Checker = j
		} else if arch == investigate.ArchDerivedJev {
			logger.Printf("ND_ARCH=derived+jev but JEV_API_KEY is unset: the check step is skipped")
		}
		if cfg.HolmesURL != "" {
			opts.Holmes = holmes.New(cfg.HolmesURL, cfg.HolmesModel, cfg.HolmesTimeout)
			if cfg.EvalHolmesModel != "" {
				opts.EvalHolmes = holmes.New(cfg.HolmesURL, cfg.EvalHolmesModel, cfg.HolmesTimeout)
				opts.EvalModel = cfg.EvalHolmesModel
			}
		}
		inv = investigate.New(ctx, opts)
	}

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: api.New(api.Options{
			Store:             store,
			ClusterName:       cfg.ClusterName,
			MaxBodyBytes:      cfg.MaxBodyBytes,
			Logger:            logger,
			Investigator:      inv,
			AutoInvestigate:   cfg.HolmesAuto,
			CorrelationWindow: cfg.CorrelationWindow,
			Eligible:          eligible,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s (data-dir=%s eval-dir=%s cluster=%s prometheus=%q holmes=%q model=%q eval-model=%q arch=%s jev=%t auto=%t skip=%q)",
			cfg.Listen, cfg.DataDir, cfg.EvalDir, cfg.ClusterName, cfg.PrometheusURL, cfg.HolmesURL, cfg.HolmesModel, cfg.EvalHolmesModel, arch,
			cfg.JevAPIKey != "", cfg.HolmesAuto, cfg.HolmesSkipRulePrefixes)
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
