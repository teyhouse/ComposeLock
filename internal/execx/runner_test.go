package execx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRedactStripsURLCredentials(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			"fatal: could not read from https://teyhouse:ghp_SUPERSECRET@github.com/x/y.git",
			"fatal: could not read from https://***@github.com/x/y.git",
		},
		{
			"remote: ssh://git:hunter2@example.com:22/repo",
			"remote: ssh://***@example.com:22/repo",
		},
		{
			"fatal: repository 'https://github.com/x/y.git' not found",
			"fatal: repository 'https://github.com/x/y.git' not found",
		},
		{
			"fatal: could not read from https://teyhouse:p@ssw@rd@github.com/x/y.git",
			"fatal: could not read from https://***@github.com/x/y.git",
		},
		{
			"see https://github.com/x/y/issues/1@2 for details",
			"see https://github.com/x/y/issues/1@2 for details",
		},
	}
	for _, tt := range tests {
		if got := RedactString(tt.in); got != tt.want {
			t.Errorf("RedactString(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := string(Redact([]byte(tt.in))); got != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRunRedactsCredentialsFromErrorAndStderr(t *testing.T) {
	const leak = "https://user:TOPSECRET@github.com/x/y.git"

	_, stderr, err := OSRunner{}.Run(t.Context(), "", nil, "sh", "-c", "echo fatal: "+leak+" >&2; exit 1")
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("credential leaked into the error: %s", err)
	}
	if strings.Contains(string(stderr), "TOPSECRET") {
		t.Errorf("credential leaked into the returned stderr: %s", stderr)
	}
	if !strings.Contains(err.Error(), "***@github.com") {
		t.Errorf("expected a redacted host in the error, got: %s", err)
	}
}

func TestRunRedactsCredentialsFromArgs(t *testing.T) {
	const leak = "https://user:TOPSECRET@github.com/x/y.git"

	_, _, err := OSRunner{}.Run(t.Context(), "", nil, "false", "clone", leak)
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("credential leaked into the error: %s", err)
	}
}

func TestRunReturnsWhenCancelledEvenIfAChildHoldsThePipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := OSRunner{}.Run(ctx, "", nil, "sh", "-c", "sleep 60 & wait")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from the cancelled command")
		}
	case <-time.After(waitDelay + 10*time.Second):
		t.Fatal("Run did not return after the context was cancelled: the WaitDelay safety net is missing")
	}
}

func TestRunSucceeds(t *testing.T) {
	stdout, _, err := OSRunner{}.Run(t.Context(), "", nil, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(stdout)) != "hello" {
		t.Errorf("stdout = %q, want %q", stdout, "hello")
	}
}

func TestRunReportsMissingBinary(t *testing.T) {
	_, _, err := OSRunner{}.Run(t.Context(), "", nil, "composelock-no-such-binary")
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("unexpected cancellation error: %v", err)
	}
}
