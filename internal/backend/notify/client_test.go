package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientPostsSafeThreadPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("missing authenticated POST")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]any{"channel": "C1", "text": "hello", "thread_ts": "10.1", "reply_broadcast": false, "unfurl_links": false, "unfurl_media": false, "parse": "none"} {
			if body[key] != want {
				t.Errorf("%s = %#v, want %#v", key, body[key], want)
			}
		}
		w.Write([]byte(`{"ok":true,"channel":"C1","ts":"10.2"}`))
	}))
	defer server.Close()
	c := NewClient("secret-token")
	c.endpoint = server.URL
	c.httpClient = server.Client()
	posted, err := c.Post(context.Background(), Message{Channel: "C1", Text: "hello", ThreadTS: "10.1"})
	if err != nil || posted.Channel != "C1" || posted.TS != "10.2" {
		t.Fatalf("posted = %#v, err = %v", posted, err)
	}
}

func TestClientClassifiesDeliveryCertainty(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		status                  int
		body, retry, kind, code string
		after                   time.Duration
	}{
		{"rate limit", 429, `{"ok":false,"error":"ratelimited"}`, "7", "retry", "ratelimited", 7 * time.Second},
		{"rate limit default", 429, ``, "bad", "retry", "ratelimited", 30 * time.Second},
		{"invalid auth", 200, `{"ok":false,"error":"invalid_auth"}`, "", "failed", "invalid_auth", 0},
		{"revoked", 200, `{"ok":false,"error":"token_revoked"}`, "", "failed", "token_revoked", 0},
		{"no channel", 200, `{"ok":false,"error":"channel_not_found"}`, "", "failed", "channel_not_found", 0},
		{"not joined", 200, `{"ok":false,"error":"not_in_channel"}`, "", "failed", "not_in_channel", 0},
		{"scope", 200, `{"ok":false,"error":"missing_scope"}`, "", "failed", "missing_scope", 0},
		{"unknown api", 200, `{"ok":false,"error":"unknown-secret-token"}`, "", "unknown", "api_error", 0},
		{"upstream", 503, `oops secret-token`, "", "unknown", "http_status", 0},
		{"malformed", 200, `no secret-token`, "", "unknown", "invalid_response", 0},
		{"missing ts", 200, `{"ok":true,"channel":"C1"}`, "", "unknown", "invalid_response", 0},
		{"wrong channel", 200, `{"ok":true,"channel":"C2","ts":"1.1"}`, "", "unknown", "invalid_response", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.retry)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := NewClient("secret-token")
			c.endpoint = s.URL
			c.httpClient = s.Client()
			_, err := c.Post(context.Background(), Message{Channel: "C1", Text: "hello"})
			var de *DeliveryError
			if !errors.As(err, &de) || de.Kind != tc.kind || de.Code != tc.code || de.RetryAfter != tc.after {
				t.Fatalf("err = %#v, want %s/%s/%s", err, tc.kind, tc.code, tc.after)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("error leaks secret")
			}
		})
	}
}

func TestClientRetriesProvableDialFailureButNotAmbiguousTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		transport  http.RoundTripper
	}{
		{"dial", "retry", &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused secret-token")}
		}}},
		{"dial timeout", "retry", &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
		}}},
		{"post-write timeout", "unknown", roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}
		})},
		{"timeout", "unknown", roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("secret-token")
			c.httpClient = &http.Client{Transport: tc.transport}
			_, err := c.Post(context.Background(), Message{Channel: "C1", Text: "hello"})
			var de *DeliveryError
			if !errors.As(err, &de) || de.Kind != tc.kind {
				t.Fatalf("err = %#v", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("secret leaked")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
