package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func testServer(t *testing.T, secret string, reconciled chan<- struct{}) *Server {
	t.Helper()
	return &Server{
		Path:   "/webhook",
		Secret: secret,
		Reconcile: func(_ context.Context) {
			reconciled <- struct{}{}
		},
		Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
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

	select {
	case <-reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconcile was not invoked")
	}
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

	resp, err := http.Post(srv.URL+"/webhook", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	select {
	case <-reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconcile was not invoked")
	}
}
