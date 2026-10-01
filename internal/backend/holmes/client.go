// Package holmes calls the HolmesGPT HTTP API and turns its answer into the
// NetworkDoctor common RCA schema.
//
// Holmes 0.38 only accepts free text (`ask`) on /api/chat. The backend does
// not pick a skill: it passes the incident data (labels carry rule_id and
// scenario) and the skills' descriptions route the investigation. The skill
// output schema is requested explicitly so the answer can be parsed.
package holmes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"networkdoctor-agent/internal/backend/incident"
)

// Client is a HolmesGPT API client.
type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// New returns a client whose HTTP timeout bounds one investigation.
func New(baseURL, model string, timeout time.Duration) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Model: model, HTTP: &http.Client{Timeout: timeout}}
}

// Answer is the useful part of an /api/chat response.
type Answer struct {
	Analysis  string
	ToolCalls int
}

type chatRequest struct {
	Ask   string `json:"ask"`
	Model string `json:"model,omitempty"`
}

type chatResponse struct {
	Analysis  string            `json:"analysis"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

// Ask sends one investigation request.
func (c *Client) Ask(ctx context.Context, ask string) (Answer, error) {
	body, _ := json.Marshal(chatRequest{Ask: ask, Model: c.Model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("holmes /api/chat: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Answer{}, fmt.Errorf("holmes /api/chat HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Answer{}, fmt.Errorf("decode holmes response: %w", err)
	}
	return Answer{Analysis: cr.Analysis, ToolCalls: len(cr.ToolCalls)}, nil
}

// BuildAsk renders the investigation request from an incident. No skill name
// is given: routing is the skills' job (their descriptions start with the
// rule_id/scenario label values).
func BuildAsk(inc *incident.Incident, windowStart, windowEnd time.Time) string {
	var b strings.Builder
	b.WriteString("A NetworkDoctor alert fired. Investigate the root cause with the matching NetworkDoctor skill, ")
	b.WriteString("Prometheus and read-only Kubernetes tools.\n")
	b.WriteString("Reply with a short summary, then exactly one fenced ```yaml block that follows the skill's Output Schema ")
	b.WriteString("(rule_id, scenario, investigation_status, confidence, time_window, affected_resources, root_cause, ")
	b.WriteString("trigger_evidence, supporting_evidence, excluded_alternatives, recommended_actions, additional_checks).\n")
	b.WriteString("Do not reply with a plan or a status update: keep calling tools until the investigation is finished, ")
	b.WriteString("and only then send one final reply that ends with the yaml block.\n\n")

	fmt.Fprintf(&b, "incident_id: %s\n", inc.IncidentID)
	fmt.Fprintf(&b, "cluster: %s\n", inc.Cluster)
	fmt.Fprintf(&b, "alert_status: %s\n", inc.AlertStatus)
	fmt.Fprintf(&b, "starts_at: %s\n", inc.StartsAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "investigation_window: %s to %s\n", windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
	b.WriteString("labels:\n")
	for _, k := range sortedKeys(inc.AlertLabels) {
		fmt.Fprintf(&b, "  %s: %q\n", k, inc.AlertLabels[k])
	}
	if len(inc.AlertAnnotations) > 0 {
		b.WriteString("annotations:\n")
		for _, k := range sortedKeys(inc.AlertAnnotations) {
			fmt.Fprintf(&b, "  %s: %q\n", k, inc.AlertAnnotations[k])
		}
	}
	if len(inc.EvidenceMetrics) > 0 {
		b.WriteString("\nevidence already collected by NetworkDoctor over the window (verify, do not trust blindly):\n")
		for _, e := range inc.EvidenceMetrics {
			fmt.Fprintf(&b, "- %s: %s\n  query: %s\n", e.Metric, e.Observation, e.Query)
		}
	}
	return b.String()
}

// RetryAsk is sent when the first answer had no parseable result (some
// models stop after announcing their plan).
func RetryAsk(original, previous string) string {
	return original + "\n\nYour previous reply ended before the investigation was done:\n---\n" +
		truncate(previous, 1500) + "\n---\nContinue the investigation now with tools and finish with the yaml block."
}

var yamlBlock = regexp.MustCompile("(?s)```ya?ml\\s*\\n(.*?)```")

// ParseResult extracts the last fenced YAML block and decodes it. It returns
// an error when there is no block or it does not look like the RCA schema.
func ParseResult(analysis string) (map[string]any, error) {
	m := yamlBlock.FindAllStringSubmatch(analysis, -1)
	if len(m) == 0 {
		return nil, fmt.Errorf("no yaml block in Holmes answer")
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(m[len(m)-1][1]), &out); err != nil {
		return nil, fmt.Errorf("decode yaml block: %w", err)
	}
	if _, ok := out["root_cause"]; !ok {
		return nil, fmt.Errorf("yaml block has no root_cause field")
	}
	return normalize(out).(map[string]any), nil
}

// normalize turns yaml.v3 maps into JSON-encodable map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			t[k] = normalize(vv)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[fmt.Sprint(k)] = normalize(vv)
		}
		return m
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	default:
		return v
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
