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

	// PrometheusURL is the Prometheus HTTP API base URL used to collect
	// evidence around an alert. Empty disables evidence collection.
	PrometheusURL string
	// HolmesURL is the HolmesGPT HTTP API base URL. Empty disables RCA.
	HolmesURL string
	// EvidenceWindow is the default look-back/look-ahead window used when
	// collecting PromQL evidence around an alert.
	EvidenceWindow time.Duration

	// MaxBodyBytes caps the Alertmanager webhook request body.
	MaxBodyBytes int64

	// HolmesModel is the Holmes modelList name to request ("" = Holmes default).
	HolmesModel string
	// HolmesTimeout bounds one investigation call.
	HolmesTimeout time.Duration
	// HolmesAuto starts an investigation automatically for new firing incidents.
	HolmesAuto bool
	// HolmesSkipRulePrefixes lists rule_id prefixes that never get an automatic
	// investigation (comma separated), e.g. "smoke-" for pipeline smoke tests.
	HolmesSkipRulePrefixes string
	// Workers is the number of concurrent investigations.
	Workers int
	// CorrelationWindow groups incidents on the same node/service that start
	// within this window; 0 disables grouping.
	CorrelationWindow time.Duration
	// GroupWait delays a group's investigation so co-firing alerts join first.
	GroupWait time.Duration
}

// ParseFlags parses CLI flags. Environment variables ND_LISTEN, ND_DATA_DIR,
// ND_CLUSTER_NAME, ND_PROMETHEUS_URL, ND_HOLMES_URL, ND_EVIDENCE_WINDOW,
// ND_MAX_BODY_BYTES, ND_HOLMES_MODEL, ND_HOLMES_TIMEOUT, ND_HOLMES_AUTO,
// ND_HOLMES_SKIP_RULE_PREFIXES and ND_WORKERS provide defaults when the flag
// is not given.
func ParseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", envOr("ND_LISTEN", ":8080"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data-dir", envOr("ND_DATA_DIR", "./data/incidents"), "directory for incident JSON files")
	flag.StringVar(&cfg.ClusterName, "cluster-name", envOr("ND_CLUSTER_NAME", "unknown"), "cluster name stamped into incidents")
	flag.StringVar(&cfg.PrometheusURL, "prometheus-url", envOr("ND_PROMETHEUS_URL", ""), "Prometheus HTTP API base URL (evidence collection, next milestone)")
	flag.StringVar(&cfg.HolmesURL, "holmes-url", envOr("ND_HOLMES_URL", ""), "HolmesGPT HTTP API base URL (RCA, next milestone)")
	flag.DurationVar(&cfg.EvidenceWindow, "evidence-window", envDurationOr("ND_EVIDENCE_WINDOW", 15*time.Minute), "PromQL evidence window around the alert")
	flag.Int64Var(&cfg.MaxBodyBytes, "max-body-bytes", envInt64Or("ND_MAX_BODY_BYTES", 4<<20), "maximum webhook request body size in bytes")
	flag.StringVar(&cfg.HolmesModel, "holmes-model", envOr("ND_HOLMES_MODEL", ""), "Holmes model name to request (empty = Holmes default)")
	flag.DurationVar(&cfg.HolmesTimeout, "holmes-timeout", envDurationOr("ND_HOLMES_TIMEOUT", 10*time.Minute), "timeout for one Holmes investigation")
	flag.BoolVar(&cfg.HolmesAuto, "holmes-auto", envOr("ND_HOLMES_AUTO", "true") == "true", "investigate new firing incidents automatically")
	flag.StringVar(&cfg.HolmesSkipRulePrefixes, "holmes-skip-rule-prefixes", envOrEmpty("ND_HOLMES_SKIP_RULE_PREFIXES", "smoke-"), "comma-separated rule_id prefixes excluded from automatic investigation")
	flag.IntVar(&cfg.Workers, "workers", int(envInt64Or("ND_WORKERS", 1)), "concurrent investigations")
	flag.DurationVar(&cfg.CorrelationWindow, "correlation-window", envDurationOr("ND_CORRELATION_WINDOW", 10*time.Minute), "group incidents on the same node/service within this window (0 disables)")
	flag.DurationVar(&cfg.GroupWait, "group-wait", envDurationOr("ND_GROUP_WAIT", 90*time.Second), "wait before investigating a group so co-firing alerts join")
	flag.Parse()
	return cfg
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envOrEmpty is like envOr but keeps an explicitly empty value, so
// ND_HOLMES_SKIP_RULE_PREFIXES="" can disable the default skip list.
func envOrEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
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
