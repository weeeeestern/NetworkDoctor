package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

type recordingSender struct {
	messages []Message
	failures map[string][]error
	before   func(Message)
}

func (s *recordingSender) Post(_ context.Context, m Message) (Posted, error) {
	if s.before != nil {
		s.before(m)
	}
	s.messages = append(s.messages, m)
	if len(s.failures[m.Channel]) > 0 {
		err := s.failures[m.Channel][0]
		s.failures[m.Channel] = s.failures[m.Channel][1:]
		if err != nil {
			return Posted{}, err
		}
	}
	return Posted{Channel: m.Channel, TS: time.Unix(int64(len(s.messages)), 0).Format("150405")}, nil
}

type workerFixture struct {
	t      *testing.T
	store  *incident.FileStore
	sender *recordingSender
	now    time.Time
	worker *Worker
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	store, err := incident.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &workerFixture{t: t, store: store, sender: &recordingSender{failures: map[string][]error{}}, now: time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)}
	f.worker = NewWorker(Options{Store: store, Sender: f.sender, Now: func() time.Time { return f.now }})
	return f
}
func (f *workerFixture) add(key, channel string) *incident.Incident {
	i := &incident.Incident{IncidentID: incident.IDForKey(key), StartsAt: f.now, SymptomSummary: "packet loss <!here>", AffectedNodes: []string{"node&1"}, RecoveryStatus: "open"}
	if channel != "" {
		i.SlackNotification = &incident.SlackNotification{ChannelID: channel, EnrolledAt: f.now, Parent: incident.NotificationDelivery{State: "pending"}}
	}
	if err := f.store.Save(i); err != nil {
		f.t.Fatal(err)
	}
	return i
}
func (f *workerFixture) scan() {
	f.t.Helper()
	if err := f.worker.Scan(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *workerFixture) tick() { f.now = f.now.Add(time.Second); f.scan() }
func (f *workerFixture) get(id string) *incident.Incident {
	f.t.Helper()
	i, err := f.store.Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return i
}
func (f *workerFixture) done(id string, attempt int) {
	f.t.Helper()
	_, err := f.store.Update(id, func(i *incident.Incident) error {
		i.HolmesStatus = "done"
		i.HolmesAttempts = attempt
		i.HolmesResult = map[string]any{"root_cause": "queue <@U1> & <!channel>", "confidence": "high", "investigation_status": "confirmed"}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestWorkerPersistsBeforeHTTPAndSendsOneParentForRepeatedScans(t *testing.T) {
	f := newWorkerFixture(t)
	i := f.add("one", "C1")
	f.sender.before = func(m Message) {
		receipt := f.get(i.IncidentID).SlackNotification.Parent
		if receipt.State != "sending" || receipt.Attempts != 1 || receipt.Payload != encodeCard(m.Text, m.Attachments) {
			t.Fatalf("not durably claimed: %#v", receipt)
		}
		_, err := f.store.Update(i.IncidentID, func(i *incident.Incident) error { i.RecoveryStatus = "resolved"; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	f.scan()
	f.tick()
	f.tick()
	got := f.get(i.IncidentID)
	if len(f.sender.messages) != 1 || got.SlackNotification.Parent.State != "sent" || got.SlackNotification.Parent.TS == "" {
		t.Fatalf("receipt = %#v, messages %d", got.SlackNotification, len(f.sender.messages))
	}
	if got.RecoveryStatus != "resolved" {
		t.Fatal("ack overwrote concurrent recovery update")
	}
	text := allText(f.sender.messages[0])
	if !strings.Contains(text, "장애 감지") || !strings.Contains(text, "&lt;!here&gt;") || !strings.Contains(text, "node&amp;1") || strings.Contains(text, "추정 원인") {
		t.Fatalf("unsafe or premature parent: %s", text)
	}
}

func TestWorkerFastAnalysisWaitsForParentAndOwnThread(t *testing.T) {
	f := newWorkerFixture(t)
	i := f.add("fast", "C1")
	f.done(i.IncidentID, 1)
	f.scan()
	if len(f.sender.messages) != 1 || f.sender.messages[0].ThreadTS != "" {
		t.Fatal("analysis preceded parent")
	}
	f.tick()
	f.tick()
	got := f.get(i.IncidentID).SlackNotification
	if len(f.sender.messages) != 2 || f.sender.messages[1].ThreadTS != got.Parent.TS || got.Reply.State != "sent" || got.LastSourceAttempt != 1 {
		t.Fatalf("bad thread/receipt: %#v %#v", got, f.sender.messages)
	}
	if reply := allText(f.sender.messages[1]); strings.Contains(reply, "<@U1>") || !strings.Contains(reply, "&lt;@U1&gt;") {
		t.Fatal("analysis can mention users")
	}
}

func TestWorkerGroupMembersAndLateJoinEachHaveOwnThread(t *testing.T) {
	f := newWorkerFixture(t)
	primary := f.add("primary", "C1")
	member := f.add("member", "C1")
	member.CorrelationID = primary.IncidentID
	member.HolmesStatus = "grouped"
	if err := f.store.Save(member); err != nil {
		t.Fatal(err)
	}
	f.done(primary.IncidentID, 1)
	for n := 0; n < 6; n++ {
		f.tick()
	}
	late := f.add("late", "C1")
	late.CorrelationID = primary.IncidentID
	late.HolmesStatus = "grouped"
	late.RecoveryStatus = "resolved"
	if err := f.store.Save(late); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		f.tick()
	}
	if len(f.sender.messages) != 6 {
		t.Fatalf("got %d messages, want three parents and replies", len(f.sender.messages))
	}
	for _, id := range []string{primary.IncidentID, member.IncidentID, late.IncidentID} {
		i := f.get(id)
		r := i.SlackNotification
		if r.Parent.State != "sent" || r.Reply.State != "sent" {
			t.Fatalf("missing per-recipient delivery %#v", r)
		}
		found := false
		for _, m := range f.sender.messages {
			if m.ThreadTS == r.Parent.TS {
				found = true
				if id != primary.IncidentID && !strings.Contains(allText(m), "함께 묶인 알림의 분석 결과") {
					t.Fatal("shared analysis not labeled")
				}
				if id != primary.IncidentID && !strings.Contains(allText(m), "대표 인시던트: "+primary.IncidentID) {
					t.Fatal("shared analysis does not identify its representative source")
				}
			}
		}
		if !found {
			t.Fatal("reply is in another recipient's thread")
		}
	}
	if f.get(late.IncidentID).RecoveryStatus != "resolved" {
		t.Fatal("shared result overwrote recipient recovery")
	}
}

func TestWorkerRateLimitDoesNotBlockOtherChannelAndRespectsRetryAfter(t *testing.T) {
	f := newWorkerFixture(t)
	a := f.add("a", "C1")
	b := f.add("b", "C2")
	f.sender.failures["C1"] = []error{&DeliveryError{Kind: "retry", Code: "ratelimited", RetryAfter: 7 * time.Second}}
	f.scan()
	if f.get(a.IncidentID).SlackNotification.Parent.State != "retry" || f.get(b.IncidentID).SlackNotification.Parent.State != "sent" {
		t.Fatal("rate limit blocked another channel")
	}
	for n := 0; n < 6; n++ {
		f.tick()
	}
	if len(f.sender.messages) != 2 {
		t.Fatal("retried before Retry-After")
	}
	f.tick()
	if f.get(a.IncidentID).SlackNotification.Parent.State != "sent" || len(f.sender.messages) != 3 {
		t.Fatal("retry did not complete")
	}
}

func TestWorkerRestoresChannelRateLimitDeadlineAfterRestart(t *testing.T) {
	f := newWorkerFixture(t)
	a := f.add("limited", "C1")
	f.sender.failures["C1"] = []error{&DeliveryError{Kind: "retry", Code: "ratelimited", RetryAfter: 7 * time.Second}}
	f.scan()
	if f.get(a.IncidentID).SlackNotification.Parent.State != "retry" {
		t.Fatal("rate limit receipt was not persisted")
	}
	b := f.add("new-same-channel", "C1")
	c := f.add("other-channel", "C2")
	f.worker = NewWorker(Options{Store: f.store, Sender: f.sender, Now: func() time.Time { return f.now }})
	f.scan()
	if f.get(b.IncidentID).SlackNotification.Parent.State != "pending" || f.get(c.IncidentID).SlackNotification.Parent.State != "sent" {
		t.Fatal("restart lost the channel deadline or blocked an unrelated channel")
	}
	for n := 0; n < 6; n++ {
		f.tick()
	}
	if len(f.sender.messages) != 2 {
		t.Fatal("same channel sent before the persisted Retry-After deadline")
	}
	f.tick()
	f.tick()
	if f.get(a.IncidentID).SlackNotification.Parent.State != "sent" || f.get(b.IncidentID).SlackNotification.Parent.State != "sent" {
		t.Fatal("same-channel work did not resume after the deadline")
	}
}

func TestWorkerRestartPausesSendingAndDoesNotBackfill(t *testing.T) {
	f := newWorkerFixture(t)
	i := f.add("ambiguous", "C1")
	i.SlackNotification.Parent.State = "sending"
	i.SlackNotification.Parent.Attempts = 1
	if err := f.store.Save(i); err != nil {
		t.Fatal(err)
	}
	f.add("historic", "")
	f.scan()
	f.tick()
	if f.get(i.IncidentID).SlackNotification.Parent.State != "unknown" || len(f.sender.messages) != 0 {
		t.Fatal("ambiguous or historical incident was resent")
	}
}

func TestWorkerUnknownAndFailedPauseAndRetryUsesExponentialDelay(t *testing.T) {
	for _, kind := range []string{"unknown", "failed", "retry"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkerFixture(t)
			i := f.add("failure", "C1")
			f.sender.failures["C1"] = []error{&DeliveryError{Kind: kind, Code: "safe"}, &DeliveryError{Kind: "retry", Code: "safe"}}
			f.scan()
			f.tick()
			if len(f.sender.messages) != 1 {
				t.Fatal("delivery retried early")
			}
			f.tick()
			if kind != "retry" {
				if len(f.sender.messages) != 1 {
					t.Fatal("paused delivery retried")
				}
				return
			}
			if len(f.sender.messages) != 2 {
				t.Fatal("first retry missing")
			}
			for n := 0; n < 3; n++ {
				f.tick()
			}
			if len(f.sender.messages) != 2 {
				t.Fatal("second retry before four seconds")
			}
			f.tick()
			if f.get(i.IncidentID).SlackNotification.Parent.State != "sent" {
				t.Fatal("retry did not finish")
			}
		})
	}
}

func TestWorkerNewCompletedSourceAttemptSendsOnlyLatestAnalysis(t *testing.T) {
	f := newWorkerFixture(t)
	i := f.add("retry-analysis", "C1")
	f.done(i.IncidentID, 1)
	f.scan()
	f.tick()
	_, err := f.store.Update(i.IncidentID, func(i *incident.Incident) error { i.HolmesStatus = "running"; i.HolmesAttempts = 2; return nil })
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	_, err = f.store.Update(i.IncidentID, func(i *incident.Incident) error { i.HolmesStatus = "failed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	if len(f.sender.messages) != 2 {
		t.Fatal("old result sent again during retry")
	}
	f.done(i.IncidentID, 3)
	f.tick()
	f.tick()
	got := f.get(i.IncidentID).SlackNotification
	if len(f.sender.messages) != 3 || got.LastSourceAttempt != 3 || got.LastSourceIncidentID != i.IncidentID {
		t.Fatalf("new source not delivered once: %#v", got)
	}
}

type failAckStore struct {
	*incident.FileStore
	fail bool
}

func (s *failAckStore) Update(id string, mutate func(*incident.Incident) error) (*incident.Incident, error) {
	if s.fail {
		s.fail = false
		return nil, errors.New("disk unavailable")
	}
	return s.FileStore.Update(id, mutate)
}
func TestWorkerFailedAcknowledgementNeverBlindRetries(t *testing.T) {
	f := newWorkerFixture(t)
	i := f.add("ack", "C1")
	store := &failAckStore{FileStore: f.store}
	f.worker = NewWorker(Options{Store: store, Sender: f.sender, Now: func() time.Time { return f.now }})
	f.sender.before = func(Message) { store.fail = true }
	if err := f.worker.Scan(context.Background()); err == nil {
		t.Fatal("lost acknowledgement failure")
	}
	f.tick()
	if len(f.sender.messages) != 1 || f.get(i.IncidentID).SlackNotification.Parent.State != "sending" {
		t.Fatal("unacknowledged request retried")
	}
	f.worker = NewWorker(Options{Store: f.store, Sender: f.sender, Now: func() time.Time { return f.now }})
	f.tick()
	if f.get(i.IncidentID).SlackNotification.Parent.State != "unknown" || len(f.sender.messages) != 1 {
		t.Fatal("restart did not pause ambiguous delivery")
	}
}

func TestReplyCardReadsAsVerdictCauseAndIncidentState(t *testing.T) {
	w := NewWorker(Options{})
	inc := &incident.Incident{Scenario: "network-congestion", RecoveryStatus: incident.RecoveryOpen}
	view := incident.ResultView{Result: map[string]any{
		"investigation_status": "excluded",
		"confidence":           "high",
		"summary":              map[string]any{"cause": "front 파드가 catshop 주소를 찾지 못해 요청이 늦어졌습니다."},
	}}
	if got := w.replyText(inc, view); got != "✅ *원인 분석 완료*" {
		t.Fatalf("reply text = %q", got)
	}
	attachments := w.replyCard(inc, view)
	if len(attachments) != 1 || attachments[0].Color != colorOther {
		t.Fatalf("a rejected scenario should be one yellow card: %#v", attachments)
	}
	got := allText(Message{Attachments: attachments})
	for _, want := range []string{"*판단*\n네트워크 혼잡 아님", "*신뢰도*\n높음", "*추정 원인*\nfront 파드가 catshop 주소를 찾지 못해 요청이 늦어졌습니다.", "*장애 상태*\n아직 진행 중"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply card missing %q:\n%s", want, got)
		}
	}
}

func TestParentCardShortensNodesAndUsesKST(t *testing.T) {
	w := NewWorker(Options{})
	inc := &incident.Incident{
		Scenario:       "networkpolicy-misconfiguration",
		SymptomSummary: "Cilium이 정책으로 트래픽 차단",
		AffectedNodes:  []string{"ip-10-80-13-35.ap-northeast-2.compute.internal"},
		StartsAt:       time.Date(2026, 11, 26, 2, 0, 32, 0, time.UTC),
	}
	attachments := w.parentCard(inc)
	if len(attachments) != 1 || attachments[0].Color != colorFiring {
		t.Fatalf("parent should be one red card: %#v", attachments)
	}
	got := allText(Message{Attachments: attachments})
	for _, want := range []string{"*증상*\nCilium이 정책으로 트래픽 차단", "*노드*\nip-10-80-13-35", "*발생*\n11월 26일 11:00:32 KST"} {
		if !strings.Contains(got, want) {
			t.Fatalf("parent card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "compute.internal") {
		t.Fatalf("node name not shortened:\n%s", got)
	}
}

func TestCardPayloadRoundTripsAndKeepsPlainTextPayloads(t *testing.T) {
	attachments := []Attachment{{Color: colorFiring, Blocks: blocks(contextBlock("a & <b>"))}}
	m := decodeCard(encodeCard("title", attachments))
	if m.Text != "title" || allText(m) != "title\na & <b>" {
		t.Fatalf("round trip = %#v", m)
	}
	if m := decodeCard("*장애 감지 · old*"); m.Text != "*장애 감지 · old*" || m.Attachments != nil {
		t.Fatalf("plain payload = %#v", m)
	}
}

// allText joins the message text and every text inside its blocks.
func allText(m Message) string {
	parts := []string{m.Text}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if text, ok := v["text"].(string); ok {
				parts = append(parts, text)
			}
			for k, child := range v {
				if k != "text" {
					walk(child)
				} else if _, ok := child.(string); !ok {
					walk(child)
				}
			}
		case []map[string]any:
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	for _, a := range m.Attachments {
		for _, b := range a.Blocks {
			walk(b)
		}
	}
	return strings.Join(parts, "\n")
}
