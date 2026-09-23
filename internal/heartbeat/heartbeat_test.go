package heartbeat

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPingHitsURLAndReportsFailuresWithoutTheURL(t *testing.T) {
	var hits, status atomic.Int64
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	var logs bytes.Buffer
	p := New(srv.URL+"/ping/secret-uuid", slog.New(slog.NewTextHandler(&logs, nil)))
	p.Ping(t.Context())
	if hits.Load() != 1 || logs.Len() != 0 {
		t.Fatalf("hits = %d, logs = %q, want one silent ping", hits.Load(), logs.String())
	}

	status.Store(http.StatusNotFound)
	p.Ping(t.Context())
	if !strings.Contains(logs.String(), "heartbeat ping failed") {
		t.Errorf("logs = %q, want a failure", logs.String())
	}

	srv.Close()
	p.Ping(t.Context())
	if strings.Contains(logs.String(), "secret-uuid") {
		t.Errorf("logs leak the ping URL: %q", logs.String())
	}

	New("", slog.New(slog.DiscardHandler)).Ping(t.Context())
}
