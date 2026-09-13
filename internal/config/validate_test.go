package config

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestValidateRejectsWebhookPathThatWouldPanicServeMux(t *testing.T) {
	for _, path := range []string{"", "webhook", "hooks/deploy", "/with space"} {
		cfg := Default()
		cfg.Webhook.Path = path
		if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("Validate() accepted webhook.path %q, want an error", path)
		}
	}
}

func TestAcceptedWebhookPathRegistersOnServeMux(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ServeMux panicked on a validated webhook.path: %v", r)
		}
	}()
	http.NewServeMux().HandleFunc("POST "+cfg.Webhook.Path, func(http.ResponseWriter, *http.Request) {})
}

func TestValidateRejectsBadWebhookListen(t *testing.T) {
	for _, listen := range []string{"", "127.0.0.1", "not a host:port:8080"} {
		cfg := Default()
		cfg.Webhook.Listen = listen
		if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("Validate() accepted webhook.listen %q, want an error", listen)
		}
	}
}

func TestValidateRejectsUnknownLogFormat(t *testing.T) {
	cfg := Default()
	cfg.LogFormat = "yaml"
	err := cfg.Validate(slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "log_format") {
		t.Errorf("Validate() error = %v, want a log_format error", err)
	}
}
