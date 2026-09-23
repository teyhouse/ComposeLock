package notify

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebhookReceivesPlainJSONWithHeaders(t *testing.T) {
	var body []byte
	var auth, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		auth, contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	embed := BuildEmbed(Report{Outcome: OutcomeSuccess, Title: "ComposeLock: deployed successfully", Commit: "8e76297aa1", Branch: "main"})
	embed.AddCommitContext(CommitContext{
		RepoURL: "https://github.com/o/r",
		Commit:  CommitInfo{ID: "8e76297aa1", Subject: "bump **vaultwarden**", Author: "teyhouse"},
		Base:    "3757e91bb2",
		Count:   2,
	})
	n := New("", slog.New(slog.DiscardHandler), WithWebhook(srv.URL, map[string]string{"Authorization": "Bearer t0k3n"}))
	if !n.SendThrottled(t.Context(), "deploy", embed) {
		t.Fatal("SendThrottled reported a failed delivery")
	}

	var got WebhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not JSON: %v: %s", err, body)
	}
	if got.Status != OutcomeSuccess || got.Title != "ComposeLock: deployed successfully" || got.Commit != "8e76297aa1" || got.Fields["Branch"] != "main" {
		t.Errorf("payload = %+v", got)
	}
	if got.CommitURL != "https://github.com/o/r/commit/8e76297aa1" || got.CompareURL != "https://github.com/o/r/compare/3757e91bb2...8e76297aa1" {
		t.Errorf("links = %q, %q", got.CommitURL, got.CompareURL)
	}
	if strings.ContainsAny(got.Message+got.Fields["Commit"], "[]*`") {
		t.Errorf("markdown left in message %q or commit field %q", got.Message, got.Fields["Commit"])
	}
	if auth != "Bearer t0k3n" || contentType != "application/json" {
		t.Errorf("headers: Authorization %q, Content-Type %q", auth, contentType)
	}
}

func TestFailingWebhookIsRetriedWithoutResendingToDiscord(t *testing.T) {
	var discordPosts, webhookPosts atomic.Int64
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discordPosts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if webhookPosts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	n := New(discord.URL, slog.New(slog.DiscardHandler), WithWebhook(webhook.URL, nil))
	if n.SendThrottled(t.Context(), "alert", Embed{Title: "a"}) {
		t.Fatal("first send reported success although the webhook failed")
	}
	if !n.SendThrottled(t.Context(), "alert", Embed{Title: "a"}) {
		t.Fatal("retry reported a failure")
	}
	if discordPosts.Load() != 1 || webhookPosts.Load() != 2 {
		t.Errorf("discord got %d posts, webhook %d, want 1 and 2", discordPosts.Load(), webhookPosts.Load())
	}
}
