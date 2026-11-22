// Package notify delivers durable incident notifications to Slack.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Message struct{ Channel, Text, ThreadTS string }
type Posted struct{ Channel, TS string }
type Sender interface {
	Post(context.Context, Message) (Posted, error)
}

// DeliveryError describes delivery certainty without retaining response bodies,
// transport errors, tokens, or message contents.
type DeliveryError struct {
	Kind, Code string
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string { return fmt.Sprintf("slack delivery %s: %s", e.Kind, e.Code) }

type Client struct {
	token      string
	endpoint   string
	httpClient *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, endpoint: "https://slack.com/api/chat.postMessage", httpClient: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Post(ctx context.Context, message Message) (Posted, error) {
	payload := struct {
		Channel        string `json:"channel"`
		Text           string `json:"text"`
		ThreadTS       string `json:"thread_ts,omitempty"`
		ReplyBroadcast bool   `json:"reply_broadcast"`
		UnfurlLinks    bool   `json:"unfurl_links"`
		UnfurlMedia    bool   `json:"unfurl_media"`
		Parse          string `json:"parse"`
	}{Channel: message.Channel, Text: message.Text, ThreadTS: message.ThreadTS, Parse: "none"}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return Posted{}, &DeliveryError{Kind: "failed", Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := c.httpClient.Do(req)
	if err != nil {
		// A dial failure establishes that no connection existed and hence no
		// request could have been accepted. Other transport failures may occur
		// after Slack has committed the message and must never be blind retried.
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return Posted{}, &DeliveryError{Kind: "retry", Code: "connect_failed"}
		}
		return Posted{}, &DeliveryError{Kind: "unknown", Code: "transport_error"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		delay := 30 * time.Second
		if seconds, e := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Retry-After")), 10, 32); e == nil && seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
		return Posted{}, &DeliveryError{Kind: "retry", Code: "ratelimited", RetryAfter: delay}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Posted{}, &DeliveryError{Kind: "unknown", Code: "http_status"}
	}
	var result struct {
		OK      bool   `json:"ok"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Error   string `json:"error"`
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || json.Unmarshal(raw, &result) != nil {
		return Posted{}, &DeliveryError{Kind: "unknown", Code: "invalid_response"}
	}
	if !result.OK {
		switch result.Error {
		case "invalid_auth", "token_revoked", "not_in_channel", "channel_not_found", "missing_scope":
			return Posted{}, &DeliveryError{Kind: "failed", Code: result.Error}
		default:
			return Posted{}, &DeliveryError{Kind: "unknown", Code: "api_error"}
		}
	}
	if strings.TrimSpace(result.Channel) == "" || strings.TrimSpace(result.TS) == "" || result.Channel != message.Channel {
		return Posted{}, &DeliveryError{Kind: "unknown", Code: "invalid_response"}
	}
	return Posted{Channel: result.Channel, TS: result.TS}, nil
}
