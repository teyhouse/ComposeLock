package state

import (
	"bytes"
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

func TestSaveOmitsZeroTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, New()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(data, []byte("0001-01-01")) {
		t.Errorf("zero timestamps written to state file: %s", data)
	}
}

func TestMemStoreOnlyPersistsSavedState(t *testing.T) {
	store := &MemStore{}

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st.PendingCommit = "abc123"
	if loaded, _ := store.Load(); loaded.Pending() {
		t.Fatal("unsaved mutation leaked into MemStore")
	}

	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st.PendingCommit = "changed-after-save"
	if loaded, _ := store.Load(); loaded.PendingCommit != "abc123" {
		t.Errorf("PendingCommit = %q, want %q", loaded.PendingCommit, "abc123")
	}
}

func TestRevertInProgress(t *testing.T) {
	st := New()
	st.PendingCommit = "abc123"
	if st.RevertInProgress() {
		t.Error("pending commit that has not failed should not report a revert in progress")
	}

	st.LastFailedCommit = "abc123"
	if !st.RevertInProgress() {
		t.Error("pending commit that already failed should report a revert in progress")
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWritable(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("CheckWritable: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("CheckWritable left files behind: %v", entries)
	}

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	readOnly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := CheckWritable(filepath.Join(readOnly, "state.json")); err == nil {
		t.Error("expected an error for a read-only state directory")
	}
}
