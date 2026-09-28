// Package alertmanager models the Alertmanager webhook receiver payload
// (webhook version "4") and derives a stable idempotency key per alert.
//
// Alertmanager retries deliveries and re-sends the same alert on every
// group_interval / repeat_interval tick, so the backend must be able to
// recognise "the same alert instance" across deliveries. The key used for
// that is fingerprint + startsAt: the fingerprint identifies the label set,
// startsAt identifies the specific occurrence.
package alertmanager

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Alert is one alert inside a webhook payload.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Payload is the top-level webhook body.
type Payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// SourceKey returns the idempotency key for this alert instance.
//
// When Alertmanager supplies a fingerprint the key is
// "<fingerprint>|<startsAt RFC3339Nano UTC>". Otherwise a fallback
// fingerprint is derived from the sorted label set so the key is still
// stable across redeliveries.
func (a Alert) SourceKey() string {
	fp := a.Fingerprint
	if fp == "" {
		fp = FallbackFingerprint(a.Labels)
	}
	return fp + "|" + a.StartsAt.UTC().Format(time.RFC3339Nano)
}

// FallbackFingerprint hashes the sorted label set. It is only used when the
// payload carries no fingerprint (for example hand-crafted test requests).
func FallbackFingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "fb-" + hex.EncodeToString(sum[:8])
}

// IsResolved reports whether this alert delivery is a resolved notification.
func (a Alert) IsResolved() bool {
	return a.Status == StatusResolved
}

// EndsAtValid reports whether endsAt carries a real timestamp. Alertmanager
// sends the zero time ("0001-01-01T00:00:00Z") while an alert is still firing.
func (a Alert) EndsAtValid() bool {
	return !a.EndsAt.IsZero() && a.EndsAt.Year() > 1
}
