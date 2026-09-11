package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildEmbedColorCoding(t *testing.T) {
	tests := []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeSuccess, int(ColorSuccess)},
		{OutcomeFailure, int(ColorFailure)},
		{OutcomeRecovered, int(ColorRecovered)},
		{OutcomeDryRun, int(ColorDryRun)},
	}
	for _, tt := range tests {
		embed := BuildEmbed(Report{Outcome: tt.outcome, Duration: time.Second})
		if embed.Color != tt.want {
			t.Errorf("outcome %s: Color = %#x, want %#x", tt.outcome, embed.Color, tt.want)
		}
	}
}

func TestBuildEmbedFields(t *testing.T) {
	embed := BuildEmbed(Report{
		Outcome:     OutcomeFailure,
		Title:       "Apply failed",
		Commit:      "abcdef1234567890",
		RepoURL:     "https://github.com/example/repo",
		Branch:      "main",
		Services:    nil,
		HealthWatch: 5 * time.Minute,
		Duration:    90 * time.Second,
		Err:         errors.New("boom"),
	})

	field := func(name string) string {
		for _, f := range embed.Fields {
			if f.Name == name {
				return f.Value
			}
		}
		t.Fatalf("no field named %q in %+v", name, embed.Fields)
		return ""
	}

	if got := field("Commit"); got != "[abcdef1](https://github.com/example/repo/commit/abcdef1234567890)" {
		t.Errorf("Commit field = %q", got)
	}
	if got := field("Services"); got != "all" {
		t.Errorf("Services field = %q, want %q", got, "all")
	}
	if got := field("Error"); got != "boom" {
		t.Errorf("Error field = %q, want %q", got, "boom")
	}
}

func TestBuildEmbedTruncatesLongError(t *testing.T) {
	longErr := errors.New(string(bytes.Repeat([]byte("x"), maxErrorFieldLen+50)))
	embed := BuildEmbed(Report{Outcome: OutcomeFailure, Err: longErr})

	for _, f := range embed.Fields {
		if f.Name == "Error" && len(f.Value) > maxErrorFieldLen+len("…") {
			t.Errorf("Error field length = %d, want <= %d", len(f.Value), maxErrorFieldLen+len("…"))
		}
	}
}

func TestNotifierSendPostsPayload(t *testing.T) {
	var received Payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decoding posted payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	n := New(srv.URL, log)

	embed := BuildEmbed(Report{Outcome: OutcomeSuccess, Title: "Deployed", Duration: time.Second})
	n.Send(t.Context(), embed)

	if len(received.Embeds) != 1 {
		t.Fatalf("received %d embeds, want 1", len(received.Embeds))
	}
	if received.Embeds[0].Title != "Deployed" {
		t.Errorf("posted title = %q, want %q", received.Embeds[0].Title, "Deployed")
	}
}

func TestNotifierSendDoesNotLogWebhookToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	webhookURL := srv.URL + "/api/webhooks/123/SUPERSECRETTOKEN"
	srv.Close()

	var buf bytes.Buffer
	n := New(webhookURL, slog.New(slog.NewTextHandler(&buf, nil)))
	n.Send(t.Context(), Embed{Title: "unreachable"})

	if !strings.Contains(buf.String(), "request failed") {
		t.Fatalf("expected the failed request to be logged, got: %s", buf.String())
	}
	if strings.Contains(buf.String(), "SUPERSECRETTOKEN") {
		t.Errorf("webhook token leaked into logs: %s", buf.String())
	}
}

func TestNotifierDisabledWithoutURL(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	n := New("", log)

	if n.Enabled() {
		t.Error("expected Notifier to be disabled with empty webhook URL")
	}
	n.Send(t.Context(), Embed{Title: "should not be sent"})
}
