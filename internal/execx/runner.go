package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const waitDelay = 5 * time.Second

var credentialRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s@]+@`)

func Redact(b []byte) []byte {
	return credentialRE.ReplaceAll(b, []byte("${1}***@"))
}

func RedactString(s string) string {
	return credentialRE.ReplaceAllString(s, "${1}***@")
}

type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout, stderr []byte, err error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	errBytes := Redact(stderr.Bytes())
	if err != nil {
		cmdline := RedactString(name + " " + strings.Join(args, " "))
		err = fmt.Errorf("%s: %w (stderr: %s)", cmdline, err, bytes.TrimSpace(errBytes))
	}
	return stdout.Bytes(), errBytes, err
}
