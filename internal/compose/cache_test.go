package compose

import (
	"path/filepath"
	"testing"
)

func TestDiscoverCacheReturnsEqualResultOnRepeatedCalls(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), "services:\n  web:\n    image: nginx\n")
	writeFile(t, filepath.Join(dir, "api", "compose.yaml"), "services:\n  api:\n    image: nginx\n")

	first, err := Discover(dir, "stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := Discover(dir, "stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("cached result has %d stacks, want %d", len(second), len(first))
	}
	for i := range first {
		if first[i].ProjectName != second[i].ProjectName || first[i].Dir != second[i].Dir {
			t.Errorf("stack %d = %+v, want %+v", i, second[i], first[i])
		}
	}
}

func TestDiscoverCacheInvalidatesOnContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	writeFile(t, path, "services:\n  web:\n    image: nginx\n")

	if _, err := Discover(dir, "stack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	writeFile(t, filepath.Join(dir, "api", "compose.yaml"), "services:\n  api:\n    image: nginx\n")

	stacks, err := Discover(dir, "stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacks) != 2 {
		t.Fatalf("got %d stacks, want 2: a new subdirectory must invalidate the cache", len(stacks))
	}

	writeFile(t, path, "services:\n  web:\n    image: nginx:alpine\n")
	if _, err := Discover(dir, "stack"); err != nil {
		t.Fatalf("unexpected error after a content change: %v", err)
	}
}

func TestDiscoverCacheInvalidatesOnProjectNameChange(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), "services:\n  web:\n    image: nginx\n")

	a, err := Discover(dir, "alpha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := Discover(dir, "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a[0].ProjectName == b[0].ProjectName {
		t.Errorf("both project names = %q; the cache key must include the base project name", a[0].ProjectName)
	}
}

func TestDiscoverResultIsNotAliasedAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), "services:\n  web:\n    image: nginx\n")

	first, err := Discover(dir, "stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first[0].ProjectName = "mutated"

	second, err := Discover(dir, "stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second[0].ProjectName != "stack" {
		t.Errorf("ProjectName = %q, want %q: callers must not be able to mutate the cached stacks", second[0].ProjectName, "stack")
	}
}
