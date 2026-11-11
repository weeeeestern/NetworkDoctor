package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/links"
)

type Store interface {
	Get(string) (*incident.Incident, error)
	Update(string, func(*incident.Incident) error) (*incident.Incident, error)
	List() ([]*incident.Incident, error)
}

type Options struct {
	Store  Store
	Sender Sender
	Links  *links.Builder
	Now    func() time.Time
	Logger *log.Logger
}

// Worker has one sender loop. All network calls happen after the durable
// sending claim, outside the incident store lock.
type Worker struct {
	o             Options
	mu            sync.Mutex
	started       bool
	nextChannelAt map[string]time.Time
}

func NewWorker(o Options) *Worker {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return &Worker{o: o, nextChannelAt: map[string]time.Time{}}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Scan(ctx); err != nil {
			w.o.Logger.Print("slack notification scan failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Scan discovers only explicitly enrolled receipts; historical incidents are
// never enrolled here. Due deliveries are scheduled without sleeping, allowing
// other channels to progress while one is rate limited.
func (w *Worker) Scan(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.o.Store == nil || w.o.Sender == nil {
		return errors.New("slack notification worker is not configured")
	}
	all, err := w.o.Store.List()
	if err != nil {
		return err
	}
	if !w.started {
		// A persisted sending state has an unknowable outcome after restart.
		// Persist the pause before allowing any new traffic.
		for _, i := range all {
			if i.SlackNotification == nil {
				continue
			}
			// A 429 is channel-wide. Restore its durable deadline before any
			// incident in that channel can be selected after process restart.
			for _, d := range []incident.NotificationDelivery{i.SlackNotification.Parent, i.SlackNotification.Reply} {
				if d.State == "retry" && d.LastErrorCode == "ratelimited" && d.NextAttemptAt != nil && d.NextAttemptAt.After(w.nextChannelAt[i.SlackNotification.ChannelID]) {
					w.nextChannelAt[i.SlackNotification.ChannelID] = *d.NextAttemptAt
				}
			}
			if i.SlackNotification.Parent.State != "sending" && i.SlackNotification.Reply.State != "sending" {
				continue
			}
			_, err = w.o.Store.Update(i.IncidentID, func(cur *incident.Incident) error {
				if cur.SlackNotification == nil {
					return incident.ErrNoChange
				}
				for _, d := range []*incident.NotificationDelivery{&cur.SlackNotification.Parent, &cur.SlackNotification.Reply} {
					if d.State == "sending" {
						d.State = "unknown"
						d.LastErrorCode = "interrupted_delivery"
						d.NextAttemptAt = nil
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		w.started = true
	}
	var scanErr error
	for _, i := range all {
		if err := ctx.Err(); err != nil {
			return errors.Join(scanErr, err)
		}
		cur, err := w.o.Store.Get(i.IncidentID)
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			continue
		}
		if cur.SlackNotification == nil || cur.SlackNotification.ChannelID == "" {
			continue
		}
		if err = w.deliver(ctx, cur, false, incident.ResultView{}); err != nil {
			scanErr = errors.Join(scanErr, err)
			continue
		}
		cur, err = w.o.Store.Get(i.IncidentID)
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			continue
		}
		if cur.SlackNotification == nil || cur.SlackNotification.Parent.State != "sent" {
			continue
		}
		view := incident.ResolveResult(cur, w.o.Store.Get)
		if !view.Completed() {
			continue
		}
		if err = w.deliver(ctx, cur, true, view); err != nil {
			scanErr = errors.Join(scanErr, err)
		}
	}
	return scanErr
}

func due(d incident.NotificationDelivery, now time.Time) bool {
	return (d.State == "pending" || d.State == "retry") && (d.NextAttemptAt == nil || !now.Before(*d.NextAttemptAt))
}

func (w *Worker) deliver(ctx context.Context, snapshot *incident.Incident, reply bool, view incident.ResultView) error {
	now := w.o.Now().UTC()
	channel := snapshot.SlackNotification.ChannelID
	if now.Before(w.nextChannelAt[channel]) {
		return nil
	}
	var message Message
	var attempt int
	claimed := false
	_, err := w.o.Store.Update(snapshot.IncidentID, func(cur *incident.Incident) error {
		n := cur.SlackNotification
		if n == nil || n.ChannelID != channel {
			return incident.ErrNoChange
		}
		d := &n.Parent
		if reply {
			if n.Parent.State != "sent" || strings.TrimSpace(n.Parent.TS) == "" || !view.Completed() {
				return incident.ErrNoChange
			}
			d = &n.Reply
			// Ambiguous and permanent failures require manual reconciliation,
			// even if another investigation has since completed.
			if d.State == "unknown" || d.State == "failed" || d.State == "sending" {
				return incident.ErrNoChange
			}
			if n.LastSourceIncidentID == view.SourceIncidentID && n.LastSourceAttempt == view.SourceAttempt {
				return incident.ErrNoChange
			}
			if d.SourceIncidentID != view.SourceIncidentID || d.SourceAttempt != view.SourceAttempt {
				*d = incident.NotificationDelivery{State: "pending", SourceIncidentID: view.SourceIncidentID, SourceAttempt: view.SourceAttempt}
			}
		}
		if !due(*d, now) {
			return incident.ErrNoChange
		}
		if d.Payload == "" {
			if reply {
				d.Payload = w.replyText(cur, view)
			} else {
				d.Payload = w.parentText(cur)
			}
		}
		d.State = "sending"
		d.Attempts++
		d.NextAttemptAt = nil
		d.LastErrorCode = ""
		attempt = d.Attempts
		message = Message{Channel: channel, Text: d.Payload}
		if reply {
			message.ThreadTS = n.Parent.TS
		}
		claimed = true
		return nil
	})
	if err != nil || !claimed {
		return err
	}
	// Reserve channel spacing for attempts as well as successful deliveries.
	w.nextChannelAt[channel] = now.Add(time.Second)
	posted, sendErr := w.o.Sender.Post(ctx, message)
	if sendErr == nil && (posted.Channel != channel || strings.TrimSpace(posted.TS) == "") {
		sendErr = &DeliveryError{Kind: "unknown", Code: "invalid_response"}
	}
	var delivery *DeliveryError
	if sendErr != nil {
		if !errors.As(sendErr, &delivery) {
			delivery = &DeliveryError{Kind: "unknown", Code: "delivery_error"}
		}
		if delivery.Kind != "retry" && delivery.Kind != "failed" && delivery.Kind != "unknown" {
			delivery = &DeliveryError{Kind: "unknown", Code: "delivery_error"}
		}
		if delivery.Kind == "retry" && delivery.RetryAfter > 0 {
			until := w.o.Now().UTC().Add(delivery.RetryAfter)
			if until.After(w.nextChannelAt[channel]) {
				w.nextChannelAt[channel] = until
			}
		}
	}
	_, err = w.o.Store.Update(snapshot.IncidentID, func(cur *incident.Incident) error {
		n := cur.SlackNotification
		if n == nil {
			return incident.ErrNoChange
		}
		d := &n.Parent
		if reply {
			d = &n.Reply
		}
		if d.State != "sending" || d.Attempts != attempt || d.Payload != message.Text {
			return incident.ErrNoChange
		}
		if delivery != nil {
			d.State = delivery.Kind
			d.LastErrorCode = safeErrorCode(delivery.Code)
			if delivery.Kind == "retry" {
				delay := retryDelay(attempt)
				if delivery.RetryAfter > delay {
					delay = delivery.RetryAfter
				}
				next := w.o.Now().UTC().Add(delay)
				d.NextAttemptAt = &next
			}
			return nil
		}
		d.State = "sent"
		d.TS = posted.TS
		d.NextAttemptAt = nil
		d.LastErrorCode = ""
		if reply {
			n.LastSourceIncidentID = d.SourceIncidentID
			n.LastSourceAttempt = d.SourceAttempt
		}
		return nil
	})
	// An acknowledgement write failure deliberately leaves sending intact.
	// Retrying that HTTP call could duplicate a message already committed.
	return err
}

func retryDelay(attempt int) time.Duration {
	if attempt >= 6 {
		return 60 * time.Second
	}
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(1<<attempt) * time.Second
}

func safeErrorCode(code string) string {
	switch code {
	case "ratelimited", "connect_failed", "transport_error", "invalid_response", "http_status", "api_error", "invalid_request", "invalid_auth", "token_revoked", "not_in_channel", "channel_not_found", "missing_scope":
		return code
	default:
		return "delivery_error"
	}
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escape(text string) string { return slackEscaper.Replace(text) }

func (w *Worker) parentText(i *incident.Incident) string {
	text := fmt.Sprintf("*장애 감지 · %s*\n증상: %s\n노드: %s\n발생: %s", escape(i.ScenarioName()), escape(i.SymptomSummary), escape(strings.Join(i.AffectedNodes, ", ")), i.StartsAt.UTC().Format(time.RFC3339))
	if url := w.o.Links.For(i).Grafana; url != "" {
		text += "\n<" + escape(url) + "|Grafana에서 보기>"
	}
	return text
}

func (w *Worker) replyText(i *incident.Incident, view incident.ResultView) string {
	h := i.HeadlineFor(view.Result)
	conf, _ := view.Result["confidence"].(string)
	text := "*원인 분석 완료*"
	if view.Shared {
		text += "\n함께 묶인 알림의 분석 결과입니다 (이 알림을 따로 검증하지는 않음)"
		text += "\n대표 인시던트: " + escape(view.SourceIncidentID)
	}
	text += "\n" + escape(h.CauseLabel) + ": " + escape(h.Cause)
	if c := incident.ConfidenceName(conf); c != "" {
		text += "\n신뢰도: " + c
	}
	recovery := map[string]string{incident.RecoveryOpen: "진행 중", incident.RecoveryResolved: "복구됨"}[i.RecoveryStatus]
	text += "\n상태: " + escape(firstNonEmpty(recovery, i.RecoveryStatus))
	if url := w.o.Links.For(i).Incident; url != "" {
		text += "\n<" + escape(url) + "|레포트 보기>"
	}
	return text
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
