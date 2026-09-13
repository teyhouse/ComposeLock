package notify

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func countingServer(t *testing.T, posts *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSendThrottledSuppressesRepeatedKey(t *testing.T) {
	var posts atomic.Int64
	srv := countingServer(t, &posts)
	n := New(srv.URL, slog.New(slog.DiscardHandler))

	for range 5 {
		n.SendThrottled(t.Context(), "git-sync", Embed{Title: "git sync failed"})
	}

	if got := posts.Load(); got != 1 {
		t.Errorf("posted %d times, want 1", got)
	}
}

func TestSendThrottledLetsDifferentKeysThrough(t *testing.T) {
	var posts atomic.Int64
	srv := countingServer(t, &posts)
	n := New(srv.URL, slog.New(slog.DiscardHandler))

	n.SendThrottled(t.Context(), "git-sync", Embed{Title: "a"})
	n.SendThrottled(t.Context(), "preflight:abc", Embed{Title: "b"})
	n.SendThrottled(t.Context(), "git-sync", Embed{Title: "c"})

	if got := posts.Load(); got != 3 {
		t.Errorf("posted %d times, want 3", got)
	}
}

func TestSendThrottledEmptyKeyAlwaysSendsAndResets(t *testing.T) {
	var posts atomic.Int64
	srv := countingServer(t, &posts)
	n := New(srv.URL, slog.New(slog.DiscardHandler))

	n.SendThrottled(t.Context(), "git-sync", Embed{Title: "fail"})
	n.SendThrottled(t.Context(), "git-sync", Embed{Title: "fail again"})
	n.SendThrottled(t.Context(), "", Embed{Title: "deployed"})
	n.SendThrottled(t.Context(), "git-sync", Embed{Title: "fail after success"})

	if got := posts.Load(); got != 3 {
		t.Errorf("posted %d times, want 3 (the success must clear the suppression window)", got)
	}
}

func TestSendRetriesOnRateLimit(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	New(srv.URL, slog.New(slog.DiscardHandler)).Send(t.Context(), Embed{Title: "rate limited"})

	if got := posts.Load(); got != 2 {
		t.Errorf("posted %d times, want 2 (one 429 then one retry)", got)
	}
}

func TestSendGivesUpAfterSecondRateLimit(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Retry-After", "0.01")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	New(srv.URL, slog.New(slog.DiscardHandler)).Send(t.Context(), Embed{Title: "rate limited"})

	if got := posts.Load(); got != 2 {
		t.Errorf("posted %d times, want 2", got)
	}
}

func TestRateLimitDelayIsCapped(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"0.5", 500 * time.Millisecond},
		{"2", 2 * time.Second},
		{"9999", maxRateLimitWait},
		{"", time.Second},
		{"not-a-number", time.Second},
		{"-3", time.Second},
	}
	for _, tt := range tests {
		if got := rateLimitDelay(tt.header); got != tt.want {
			t.Errorf("rateLimitDelay(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestTruncateStaysWithinLimitAndValidUTF8(t *testing.T) {
	s := strings.Repeat("ü", 200)
	got := truncate(s, 51)

	if len(got) > 51 {
		t.Errorf("truncate returned %d bytes, want <= 51", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("truncate(%d bytes) = %q, want an ellipsis suffix", len(s), got)
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("truncate() = %q, want %q", got, "hello")
	}
}

func TestBuildEmbedCapsFieldValues(t *testing.T) {
	services := make([]string, 500)
	for i := range services {
		services[i] = "service-with-a-fairly-long-name"
	}
	embed := BuildEmbed(Report{
		Outcome:  OutcomeSuccess,
		Title:    strings.Repeat("t", 400),
		Services: services,
		Stacks:   services,
	})

	if len(embed.Title) > maxTitleLen {
		t.Errorf("title = %d bytes, want <= %d", len(embed.Title), maxTitleLen)
	}
	if len(embed.Fields) > maxFields {
		t.Errorf("fields = %d, want <= %d", len(embed.Fields), maxFields)
	}
	for _, f := range embed.Fields {
		if len(f.Value) > maxFieldValueLen {
			t.Errorf("field %q value = %d bytes, want <= %d", f.Name, len(f.Value), maxFieldValueLen)
		}
		if f.Value == "" {
			t.Errorf("field %q has an empty value, which Discord rejects", f.Name)
		}
	}
}

func TestSendDrainsResponseBody(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()

	n := New(srv.URL, slog.New(slog.NewTextHandler(&buf, nil)))
	for range 3 {
		n.Send(t.Context(), Embed{Title: "ok"})
	}
	if strings.Contains(buf.String(), "request failed") {
		t.Errorf("unexpected request failure: %s", buf.String())
	}
}
