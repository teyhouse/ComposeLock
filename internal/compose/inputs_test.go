package compose

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectInputsFollowsIncludeFragments(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - shared/base.yaml\nservices:\n  web:\n    image: nginx\n")
	write(t, filepath.Join(dir, "shared", "base.yaml"), "services:\n  db:\n    image: postgres\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	got, complete := ProjectInputs(project)
	if !complete {
		t.Fatalf("expected a complete input set, got %q", got)
	}
	if !slices.Contains(got, filepath.Join(dir, "shared", "base.yaml")) {
		t.Errorf("ProjectInputs = %q, want the included fragment", got)
	}
	if !slices.Contains(got, filepath.Join(dir, "shared", ".env")) {
		t.Errorf("ProjectInputs = %q, want the included project's implicit .env", got)
	}
}

func TestProjectInputsFollowsNestedIncludesAndExtends(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - a/compose.yaml\n")
	write(t, filepath.Join(dir, "a", "compose.yaml"), "include:\n  - ../b/compose.yaml\nservices:\n  web:\n    extends:\n      file: common.yaml\n      service: base\n")
	write(t, filepath.Join(dir, "a", "common.yaml"), "services:\n  base:\n    image: nginx\n")
	write(t, filepath.Join(dir, "b", "compose.yaml"), "services:\n  db:\n    image: postgres\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	got, complete := ProjectInputs(project)
	if !complete {
		t.Fatalf("expected a complete input set, got %q", got)
	}
	for _, want := range []string{
		filepath.Join(dir, "a", "compose.yaml"),
		filepath.Join(dir, "a", "common.yaml"),
		filepath.Join(dir, "b", "compose.yaml"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("ProjectInputs = %q, want it to contain %q", got, want)
		}
	}
}

func TestProjectInputsReportsIncompleteForUnresolvableIncludes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - ${STACK_DIR}/compose.yaml\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	if _, complete := ProjectInputs(project); complete {
		t.Error("an interpolated include path cannot be resolved statically, so the set must be reported incomplete")
	}
}

func TestProjectInputsIgnoresRemoteIncludes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - https://example.com/compose.yaml\nservices:\n  web:\n    image: nginx\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	if _, complete := ProjectInputs(project); !complete {
		t.Error("a remote include is never a repository path, so it must not make the set incomplete")
	}
}

func TestProjectInputsSurvivesAnIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - other.yaml\n")
	write(t, filepath.Join(dir, "other.yaml"), "include:\n  - docker-compose.yml\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	got, _ := ProjectInputs(project)
	if !slices.Contains(got, filepath.Join(dir, "other.yaml")) {
		t.Errorf("ProjectInputs = %q, want both files despite the cycle", got)
	}
}

func TestProjectInputsHonoursIncludeProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), "include:\n  - path: frag/compose.yaml\n    project_directory: elsewhere\n")
	write(t, filepath.Join(dir, "frag", "compose.yaml"), "services:\n  web:\n    image: nginx\n")

	project := &types.Project{
		WorkingDir:   dir,
		ComposeFiles: []string{filepath.Join(dir, "docker-compose.yml")},
	}

	got, complete := ProjectInputs(project)
	if !complete {
		t.Fatalf("expected a complete input set, got %q", got)
	}
	if !slices.Contains(got, filepath.Join(dir, "elsewhere", ".env")) {
		t.Errorf("ProjectInputs = %q, want the declared project directory's .env", got)
	}
}

func TestIsInfraError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"parse error", errors.New("yaml: mapping values are not allowed"), false},
		{"deadline", fmt.Errorf("compose up: %w", context.DeadlineExceeded), true},
		{"canceled", fmt.Errorf("compose up: %w", context.Canceled), true},
		{"connection refused", fmt.Errorf("dialing docker: %w", syscall.ECONNREFUSED), true},
		{"connection reset", fmt.Errorf("compose up: %w", syscall.ECONNRESET), true},
		{"net timeout", fmt.Errorf("compose up: %w", &net.DNSError{IsTimeout: true}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInfraError(tc.err); got != tc.want {
				t.Errorf("IsInfraError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
