// Package prometheus is a minimal Prometheus HTTP API client used to collect
// evidence around an alert. Only query_range is needed.
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Prometheus server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with a sane default timeout.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Sample is one point of a range series.
type Sample struct {
	T time.Time
	V float64
}

// Series is one labelled range series.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// QueryRange runs a range query and returns matrix series. NaN and Inf points
// are dropped.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query_range: %w", err)
	}
	defer resp.Body.Close()

	var body apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode prometheus response (HTTP %d): %w", resp.StatusCode, err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", body.ErrorType, body.Error)
	}

	out := make([]Series, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s := Series{Labels: r.Metric}
		for _, v := range r.Values {
			ts, ok1 := v[0].(float64)
			vs, ok2 := v[1].(string)
			if !ok1 || !ok2 {
				continue
			}
			f, err := strconv.ParseFloat(vs, 64)
			if err != nil || f != f || f > 1e300 || f < -1e300 {
				continue
			}
			s.Samples = append(s.Samples, Sample{T: time.Unix(int64(ts), 0).UTC(), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}
