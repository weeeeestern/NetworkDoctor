// Package check is the conclusion checker of the "derived+jev" architecture:
// after Holmes answers, an independent decision model judges whether the
// conclusion is consistent with the deterministic facts NetworkDoctor
// computed. A failed check sends Holmes back once with the facts in hand.
//
// The checker only adds a gate; it never widens what is accepted. It sits
// behind the Checker interface so it can be swapped or turned off (Jev is a
// SaaS endpoint and is not available in air-gapped clusters).
package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

// Input is what the checker sees.
type Input struct {
	Labels  map[string]string
	Derived []incident.Evidence
	Result  map[string]any
}

// Verdict is the checker's answer.
type Verdict struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Consistent is the probability (0..1) that the conclusion agrees with
	// the derived facts.
	Consistent float64 `json:"consistent"`
	// SupportedStatus is the investigation_status the facts best support.
	SupportedStatus  string             `json:"supported_status,omitempty"`
	StatusConfidence float64            `json:"status_confidence,omitempty"`
	StatusProbs      map[string]float64 `json:"status_probabilities,omitempty"`
	InputTokens      int                `json:"input_tokens,omitempty"`
	LatencyMS        int64              `json:"latency_ms"`
}

// Pass reports whether the conclusion passes the gate.
func (v Verdict) Pass(threshold float64) bool { return v.Consistent >= threshold }

// Checker judges a conclusion.
type Checker interface {
	Check(ctx context.Context, in Input) (Verdict, error)
}

// Jev calls TypeSafe's System One API (https://docs.typesafe.ai/api).
type Jev struct {
	URL    string // https://api.typesafe.ai/v1/systemone
	Model  string // jev-latest
	APIKey string
	HTTP   *http.Client
}

// NewJev returns a Jev checker, or nil when no API key is configured.
func NewJev(url, model, apiKey string) *Jev {
	if apiKey == "" {
		return nil
	}
	if url == "" {
		url = "https://api.typesafe.ai/v1/systemone"
	}
	if model == "" {
		model = "jev-latest"
	}
	return &Jev{URL: url, Model: model, APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type jevRequest struct {
	State     any                    `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// BuildState renders the checker state. Exported for tests and for the
// eval harness, which records exactly what the checker saw.
func BuildState(in Input) map[string]any {
	facts := make([]string, 0, len(in.Derived))
	for _, e := range in.Derived {
		facts = append(facts, strings.TrimPrefix(e.Metric, "derived:")+": "+e.Observation)
	}
	r := in.Result
	return map[string]any{
		"alert": map[string]string{
			"alertname": in.Labels["alertname"], "rule_id": in.Labels["rule_id"],
			"scenario": in.Labels["scenario"], "variant": in.Labels["variant"],
			"node": in.Labels["node"], "service": in.Labels["service"],
			"retransmit_node": in.Labels["retransmit_node"],
		},
		"derived_facts": facts,
		"conclusion": map[string]any{
			"investigation_status": r["investigation_status"],
			"confidence":           r["confidence"],
			"root_cause":           r["root_cause"],
			"affected_resources":   r["affected_resources"],
		},
		"notes": "derived_facts are computed deterministically from Prometheus by code. " +
			"A fact that says 'unavailable' means the data was missing, not that the value was normal.",
	}
}

// Check asks two questions in one call: is the conclusion consistent with
// the facts (noul), and which status do the facts support (choice).
func (j *Jev) Check(ctx context.Context, in Input) (Verdict, error) {
	body, _ := json.Marshal(jevRequest{
		State: BuildState(in),
		Model: j.Model,
		Questions: map[string]jevQuestion{
			"consistent": {
				Type: "noul",
				Instructions: "Is `conclusion` consistent with every item in `derived_facts`? " +
					"Inconsistent means it names a different node, pod or cause than the facts point to, " +
					"ignores a fact's Reading, or marks a cause excluded that the facts support.",
				Criteria: map[string]string{
					"true":  "The conclusion agrees with all derived facts.",
					"false": "The conclusion contradicts or ignores at least one derived fact.",
				},
			},
			"supported_status": {
				Type:         "choice",
				Instructions: "Which investigation_status do `derived_facts` best support for this alert?",
				Criteria: map[string]string{
					"confirmed":    "The facts support the alert's scenario as the root cause.",
					"excluded":     "The facts show the alert's scenario is not the cause.",
					"inconclusive": "The facts neither confirm nor exclude the scenario.",
				},
			},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+j.APIKey)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("jev HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var jr jevResponse
	if err := json.Unmarshal(raw, &jr); err != nil {
		return Verdict{}, fmt.Errorf("decode jev response: %w", err)
	}
	c, ok := jr.Answers["consistent"]
	if !ok {
		return Verdict{}, fmt.Errorf("jev response has no 'consistent' answer")
	}
	s := jr.Answers["supported_status"]
	return Verdict{
		Provider: "jev", Model: jr.Model,
		Consistent:      c.Noul,
		SupportedStatus: s.Choice, StatusConfidence: s.Confidence, StatusProbs: s.Probabilities,
		InputTokens: jr.Usage.InputTokens,
		LatencyMS:   time.Since(t0).Milliseconds(),
	}, nil
}

// Feedback is appended to the Holmes request when the check fails.
func Feedback(in Input, v Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nAn independent checker found your previous conclusion inconsistent with NetworkDoctor's derived facts "+
		"(consistency %.2f; the facts best support investigation_status=%s).\n", v.Consistent, orDash(v.SupportedStatus))
	b.WriteString("Your previous conclusion:\n")
	fmt.Fprintf(&b, "  investigation_status: %v\n  root_cause: %v\n", in.Result["investigation_status"], in.Result["root_cause"])
	b.WriteString("Re-examine it against these facts, verify them with tools, and finish with a corrected yaml block. ")
	b.WriteString("Keep your conclusion only if you can show with evidence why a fact does not apply.\n")
	for _, e := range in.Derived {
		fmt.Fprintf(&b, "- %s: %s\n", strings.TrimPrefix(e.Metric, "derived:"), e.Observation)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
