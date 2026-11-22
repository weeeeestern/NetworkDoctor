package incident

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestResolveResultSharesOneHopWithoutChangingRecovery(t *testing.T) {
	primary := &Incident{IncidentID: "primary", CorrelationID: "primary", HolmesStatus: "done", HolmesAttempts: 2, HolmesResult: map[string]any{"root_cause": "packet loss"}, RecoveryStatus: RecoveryResolved}
	member := &Incident{IncidentID: "member", CorrelationID: "primary", RecoveryStatus: RecoveryOpen}
	before, _ := json.Marshal(member)
	got := ResolveResult(member, func(id string) (*Incident, error) {
		if id != "primary" {
			t.Fatalf("lookup id = %q", id)
		}
		return primary, nil
	})
	if got.Source != primary || got.SourceIncidentID != "primary" || got.SourceHolmesStatus != "done" || got.SourceAttempt != 2 || !got.Shared || got.Previous || got.Unavailable || !got.Completed() {
		t.Fatalf("shared view = %+v", got)
	}
	if got.Result["root_cause"] != "packet loss" {
		t.Fatalf("result = %v", got.Result)
	}
	after, _ := json.Marshal(member)
	if string(before) != string(after) || member.RecoveryStatus != RecoveryOpen {
		t.Fatal("result resolution changed member identity or recovery")
	}
}

func TestResolveResultOwnAttemptOrResultWinsOverGroup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		result   map[string]any
	}{
		{"failed attempt", 1, nil},
		{"legacy result", 0, map[string]any{"root_cause": "old result"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inc := &Incident{IncidentID: "member", CorrelationID: "primary", HolmesStatus: "failed", HolmesAttempts: tc.attempts, HolmesResult: tc.result}
			got := ResolveResult(inc, func(string) (*Incident, error) { t.Fatal("independent member looked up primary"); return nil, nil })
			if got.Source != inc || got.SourceIncidentID != "member" || got.Shared || got.Unavailable || got.SourceHolmesStatus != "failed" || got.SourceAttempt != tc.attempts || !reflect.DeepEqual(got.Result, tc.result) {
				t.Fatalf("own view = %+v", got)
			}
		})
	}
}

func TestResolveResultRejectsUnavailableOrIndirectRepresentative(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source *Incident
		err    error
	}{
		{"missing", nil, errors.New("not found")},
		{"nil", nil, nil},
		{"member", &Incident{IncidentID: "primary", CorrelationID: "other", HolmesStatus: "done", HolmesResult: map[string]any{"root_cause": "wrong"}}, nil},
		{"wrong id", &Incident{IncidentID: "other", HolmesStatus: "done", HolmesResult: map[string]any{"root_cause": "wrong"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := ResolveResult(&Incident{IncidentID: "member", CorrelationID: "primary"}, func(string) (*Incident, error) { calls++; return tc.source, tc.err })
			if !got.Unavailable || got.SourceHolmesStatus != "unavailable" || got.Result != nil || got.Completed() || calls != 1 {
				t.Fatalf("unavailable view = %+v, calls = %d", got, calls)
			}
		})
	}
	got := ResolveResult(&Incident{IncidentID: "member", CorrelationID: "primary"}, nil)
	if !got.Unavailable {
		t.Fatalf("nil lookup view = %+v", got)
	}
}

func TestResolveResultPreviousAndCompletedRequireUsableCurrentResult(t *testing.T) {
	for _, tc := range []struct {
		status              string
		result              map[string]any
		previous, completed bool
	}{
		{"done", map[string]any{"root_cause": "usable"}, false, true},
		{"done", map[string]any{"root_cause": " \n\t"}, false, false},
		{"done", map[string]any{"root_cause": 17}, false, false},
		{"done", nil, false, false},
		{"queued", map[string]any{"root_cause": "previous"}, true, false},
		{"running", map[string]any{"root_cause": "previous"}, true, false},
		{"failed", map[string]any{"root_cause": "previous"}, true, false},
		{"failed", nil, false, false},
		{"skipped", map[string]any{"root_cause": "old"}, false, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			got := ResolveResult(&Incident{IncidentID: "own", HolmesStatus: tc.status, HolmesResult: tc.result}, nil)
			if got.Previous != tc.previous || got.Completed() != tc.completed {
				t.Fatalf("view = %+v, completed = %v", got, got.Completed())
			}
		})
	}
	if got := ResolveResult(nil, nil); !got.Unavailable || got.Completed() {
		t.Fatalf("nil view = %+v", got)
	}
}

func TestResultAndNotificationJSONContract(t *testing.T) {
	view := ResolveResult(&Incident{IncidentID: "own", HolmesStatus: "done", HolmesAttempts: 3, HolmesResult: map[string]any{"root_cause": "cause"}}, nil)
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"source_incident_id", "source_holmes_status", "source_attempt", "shared", "previous", "unavailable", "result"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing field %q in %s", key, data)
		}
	}
	if len(fields) != 7 {
		t.Fatalf("internal source leaked: %s", data)
	}
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	inc := &Incident{SlackNotification: &SlackNotification{ChannelID: "C1", EnrolledAt: now, Parent: NotificationDelivery{State: "sent", TS: "123.4", Attempts: 2, NextAttemptAt: &now, LastErrorCode: "ratelimited", Payload: "frozen", SourceIncidentID: "primary", SourceAttempt: 3}, Reply: NotificationDelivery{State: "pending"}, LastSourceIncidentID: "primary", LastSourceAttempt: 3}}
	data, err = json.Marshal(inc)
	if err != nil {
		t.Fatal(err)
	}
	var restored Incident
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.SlackNotification, inc.SlackNotification) {
		t.Fatalf("notification roundtrip = %+v", restored.SlackNotification)
	}
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	notif := fields["slack_notification"].(map[string]any)
	if notif["channel_id"] != "C1" || notif["last_source_incident_id"] != "primary" || notif["last_source_attempt"] != float64(3) {
		t.Fatalf("notification JSON = %s", data)
	}
}
