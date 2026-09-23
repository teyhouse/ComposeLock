package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func startWorker(t *testing.T, s *Server) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Go(func() { s.processTriggers(t.Context()) })
	t.Cleanup(wg.Wait)
}

func testServer(t *testing.T, secret string, reconciled chan<- struct{}) *Server {
	t.Helper()
	s := New(Config{Path: "/webhook", Secret: secret}, func(context.Context) {
		reconciled <- struct{}{}
	}, slog.New(slog.DiscardHandler))
	startWorker(t, s)
	return s
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func postEmpty(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

func TestWebhookValidSignatureAccepted(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "shh", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("shh", body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	waitFor(t, reconciled, "Reconcile to be invoked")
}

func TestWebhookInvalidSignatureRejected(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "shh", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	select {
	case <-reconciled:
		t.Fatal("Reconcile should not be invoked on invalid signature")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWebhookMissingSignatureRejected(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "shh", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestWebhookNoSecretSkipsValidation(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postEmpty(t, srv.URL+"/webhook")
	waitFor(t, reconciled, "Reconcile to be invoked")
}

func TestWebhookCoalescesTriggersWhileReconciling(t *testing.T) {
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	s := New(Config{Path: "/webhook"}, func(ctx context.Context) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}, slog.New(slog.DiscardHandler))
	startWorker(t, s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postEmpty(t, srv.URL+"/webhook")
	waitFor(t, started, "first reconcile")

	for range 3 {
		postEmpty(t, srv.URL+"/webhook")
	}
	release <- struct{}{}
	waitFor(t, started, "follow-up reconcile for pushes received mid-run")
	release <- struct{}{}

	select {
	case <-started:
		t.Fatal("expected pushes received during a reconcile to coalesce into a single follow-up run")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServeWaitsForInFlightReconcileOnShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	var finished atomic.Bool
	s := New(Config{Path: "/webhook"}, func(ctx context.Context) {
		started <- struct{}{}
		<-ctx.Done()
		finished.Store(true)
	}, slog.New(slog.DiscardHandler))

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, ln) }()

	postEmpty(t, "http://"+ln.Addr().String()+"/webhook")
	waitFor(t, started, "reconcile to start")
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if !finished.Load() {
		t.Error("Serve returned before the in-flight reconcile finished")
	}
}

func TestWebhookIgnoresNonPushEvents(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	select {
	case <-reconciled:
		t.Fatal("an issue_comment event must not trigger a deploy")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWebhookAnswersPingWithoutTriggering(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := testServer(t, "", reconciled)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-GitHub-Event", "ping")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	select {
	case <-reconciled:
		t.Fatal("a ping must not trigger a deploy")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWebhookIgnoresPushesToOtherBranches(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	s := New(Config{Path: "/webhook", Branch: "main"}, func(context.Context) {
		reconciled <- struct{}{}
	}, slog.New(slog.DiscardHandler))
	startWorker(t, s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(ref string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/webhook", "application/json", bytes.NewReader([]byte(`{"ref":"`+ref+`"}`)))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
	}

	post("refs/heads/feature")
	select {
	case <-reconciled:
		t.Fatal("a push to another branch must not trigger a deploy")
	case <-time.After(100 * time.Millisecond):
	}

	post("refs/heads/main")
	waitFor(t, reconciled, "Reconcile for a push to the tracked branch")
}

func TestProcessTriggersSurvivesAPanickingReconcile(t *testing.T) {
	calls := make(chan int, 2)
	n := 0
	s := New(Config{Path: "/webhook"}, func(context.Context) {
		n++
		calls <- n
		if n == 1 {
			panic("boom")
		}
	}, slog.New(slog.DiscardHandler))
	startWorker(t, s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postEmpty(t, srv.URL+"/webhook")
	if got := <-calls; got != 1 {
		t.Fatalf("first call = %d, want 1", got)
	}
	postEmpty(t, srv.URL+"/webhook")
	select {
	case got := <-calls:
		if got != 2 {
			t.Fatalf("second call = %d, want 2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker died on the first panic instead of recovering")
	}
}

func TestHealthzAnswersOK(t *testing.T) {
	s := New(Config{Path: "/webhook"}, func(context.Context) {}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}
