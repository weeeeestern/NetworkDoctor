// Package config holds runtime configuration for the NetworkDoctor backend.
//
// Every knob is a CLI flag with an ND_* environment variable fallback so the
// same binary works from a shell, a Dockerfile, and a Kubernetes Deployment
// without a config file.
package config

import (
	"flag"
	"os"
	"strconv"
	"time"
)

// Config is the backend runtime configuration.
type Config struct {
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string
	// DataDir is the directory where incident JSON files are persisted.
	DataDir string
	// ClusterName is stamped into every incident so reports from several
	// clusters can be told apart later.
	ClusterName string

	// PrometheusURL is the in-cluster Prometheus HTTP API base URL. It is
	// parsed now so the Deployment contract is stable; evidence collection
	// that uses it lands in the next milestone.
	PrometheusURL string
	// HolmesURL is the HolmesGPT HTTP API base URL. Same note as above.
	HolmesURL string
	// EvidenceWindow is the default look-back/look-ahead window used when
	// collecting PromQL evidence around an alert.
	EvidenceWindow time.Duration

	// MaxBodyBytes caps the Alertmanager webhook request body.
	MaxBodyBytes int64
}

// ParseFlags parses CLI flags. Environment variables ND_LISTEN, ND_DATA_DIR,
// ND_CLUSTER_NAME, ND_PROMETHEUS_URL, ND_HOLMES_URL, ND_EVIDENCE_WINDOW and
// ND_MAX_BODY_BYTES provide defaults when the flag is not given.
func ParseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", envOr("ND_LISTEN", ":8080"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data-dir", envOr("ND_DATA_DIR", "./data/incidents"), "directory for incident JSON files")
	flag.StringVar(&cfg.ClusterName, "cluster-name", envOr("ND_CLUSTER_NAME", "unknown"), "cluster name stamped into incidents")
	flag.StringVar(&cfg.PrometheusURL, "prometheus-url", envOr("ND_PROMETHEUS_URL", ""), "Prometheus HTTP API base URL (evidence collection, next milestone)")
	flag.StringVar(&cfg.HolmesURL, "holmes-url", envOr("ND_HOLMES_URL", ""), "HolmesGPT HTTP API base URL (RCA, next milestone)")
	flag.DurationVar(&cfg.EvidenceWindow, "evidence-window", envDurationOr("ND_EVIDENCE_WINDOW", 15*time.Minute), "PromQL evidence window around the alert")
	flag.Int64Var(&cfg.MaxBodyBytes, "max-body-bytes", envInt64Or("ND_MAX_BODY_BYTES", 4<<20), "maximum webhook request body size in bytes")
	flag.Parse()
	return cfg
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt64Or(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
