package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsFreshState(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.SchemaVersion != schemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", st.SchemaVersion, schemaVersion)
	}
	if st.Pending() {
		t.Error("fresh state should not be pending")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	want := New()
	want.LastHealthyCommit = "abc123"
	want.LastHealthyAt = time.Now().UTC().Truncate(time.Second)
	want.LastResult = ResultSuccess
	want.PendingCommit = "def456"

	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.LastHealthyCommit != want.LastHealthyCommit {
		t.Errorf("LastHealthyCommit = %q, want %q", got.LastHealthyCommit, want.LastHealthyCommit)
	}
	if !got.LastHealthyAt.Equal(want.LastHealthyAt) {
		t.Errorf("LastHealthyAt = %v, want %v", got.LastHealthyAt, want.LastHealthyAt)
	}
	if got.LastResult != want.LastResult {
		t.Errorf("LastResult = %q, want %q", got.LastResult, want.LastResult)
	}
	if !got.Pending() {
		t.Error("expected pending state after round trip")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := New()
	original.LastHealthyCommit = "original"
	if err := Save(path, original); err != nil {
		t.Fatalf("Save original: %v", err)
	}

	if err := Save(path, New()); err != nil {
		t.Fatalf("Save replacement: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("leftover temp file after Save: %s", e.Name())
		}
	}
}
