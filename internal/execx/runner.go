package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout, stderr []byte, err error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}
