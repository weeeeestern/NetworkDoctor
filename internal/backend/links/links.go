// Package links builds public incident and Grafana links from explicit operator
// configuration rather than internal URLs included in Alertmanager payloads.
package links

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

type Links struct {
	Grafana  string `json:"grafana,omitempty"`
	Incident string `json:"incident,omitempty"`
}

type Builder struct {
	grafana *url.URL
	public  *url.URL
}

// New validates the public base URLs. PublicURL must be an origin because UI
// assets are served from the root; Grafana may be served under a subpath.
func New(grafanaURL, publicURL string) (*Builder, error) {
	grafana, err := parseBase(grafanaURL, false)
	if err != nil {
		return nil, fmt.Errorf("grafana URL: %w", err)
	}
	public, err := parseBase(publicURL, true)
	if err != nil {
		return nil, fmt.Errorf("public URL: %w", err)
	}
	return &Builder{grafana: grafana, public: public}, nil
}

func parseBase(raw string, originOnly bool) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("must be an HTTP or HTTPS URL with a host and no credentials, query, or fragment")
	}
	if originOnly && (u.Path != "" && u.Path != "/" || u.RawPath != "") {
		return nil, fmt.Errorf("must use the origin root")
	}
	return u, nil
}

// For builds links for the requested incident's own scope and alert window.
func (b *Builder) For(inc *incident.Incident) Links {
	if b == nil || inc == nil {
		return Links{}
	}
	var out Links
	if b.public != nil {
		u := *b.public
		u.Path = "/"
		u.Fragment = "/incident/" + inc.IncidentID
		out.Incident = u.String()
	}
	if b.grafana == nil || !knownRule(inc.RuleID) {
		return out
	}
	u := b.grafana.JoinPath("d", "nd-"+inc.RuleID)
	q := make(url.Values)
	q.Set("from", strconv.FormatInt(inc.StartsAt.Add(-10*time.Minute).UnixMilli(), 10))
	q.Set("to", "now")
	if inc.EndsAt != nil {
		q.Set("to", strconv.FormatInt(inc.EndsAt.Add(10*time.Minute).UnixMilli(), 10))
	}
	if inc.RuleID != "rule-6" {
		seen := map[string]bool{}
		for _, node := range inc.AffectedNodes {
			if node != "" && !seen[node] {
				q.Add("var-node", node)
				seen[node] = true
			}
		}
		if node := inc.AlertLabels["retransmit_node"]; inc.RuleID == "rule-1" && node != "" && !seen[node] {
			q.Add("var-node", node)
		}
	}
	if service, present := inc.AlertLabels["service"]; present {
		q.Set("var-service", service)
	}
	u.RawQuery = q.Encode()
	out.Grafana = u.String()
	return out
}

func knownRule(rule string) bool {
	switch rule {
	case "rule-1", "rule-2", "rule-3", "rule-4", "rule-5", "rule-6", "rule-7", "rule-8":
		return true
	default:
		return false
	}
}
