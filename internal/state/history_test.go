package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const stateWrittenBeforePauseAndHistory = `{
  "schema_version": 2,
  "last_healthy_commit": "9f3c1a2b4d5e6f708192a3b4c5d6e7f8091a2b3c",
  "last_healthy_at": "2026-09-11T09:12:00Z",
  "last_healthy_stacks": ["my-stack"],
  "last_failed_commit": "",
  "last_result": "success",
  "last_attempt_commit": "9f3c1a2b4d5e6f708192a3b4c5d6e7f8091a2b3c",
  "pending_commit": ""
}`

func TestStateWithoutPauseOrHistoryLoadsAndGainsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(stateWrittenBeforePauseAndHistory), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Load(path)
	if err != nil {
		t.Fatalf("loading a state file from before pause and history: %v", err)
	}
	if st.Paused(time.Now()) || len(st.History) != 0 || st.LastHealthyCommit != "9f3c1a2b4d5e6f708192a3b4c5d6e7f8091a2b3c" {
		t.Fatalf("loaded %+v, want the old fields kept, not paused and no history", st)
	}

	st.Record(Deployment{At: time.Now(), Commit: "abc1234", Result: ResultSuccess})
	st.Pause(time.Now(), time.Time{}, "maintenance")
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.History) != 1 || !again.Paused(time.Now()) || again.SchemaVersion != schemaVersion {
		t.Errorf("round trip = %+v, want one history entry, paused, schema %d", again, schemaVersion)
	}
}

func TestRecordCollapsesRepeatsAndKeepsTheNewest(t *testing.T) {
	st := New()
	st.Record(Deployment{Result: ResultFailedGit})
	st.Record(Deployment{Result: ResultFailedGit})
	if len(st.History) != 1 || st.History[0].Repeats != 1 {
		t.Fatalf("history = %+v, want one entry repeated once", st.History)
	}
	for i := range maxHistory + 5 {
		st.Record(Deployment{Commit: fmt.Sprintf("c%d", i), Result: ResultSuccess})
	}
	if len(st.History) != maxHistory || st.History[maxHistory-1].Commit != fmt.Sprintf("c%d", maxHistory+4) {
		t.Errorf("history has %d entries ending with %q, want %d ending with the newest", len(st.History), st.History[len(st.History)-1].Commit, maxHistory)
	}
}

func TestPauseExpires(t *testing.T) {
	now := time.Now()
	st := New()
	st.Pause(now, now.Add(time.Hour), "")
	if !st.Paused(now) || st.Paused(now.Add(2*time.Hour)) {
		t.Error("a timed pause should hold before its end and lapse after it")
	}
	st.Resume()
	if st.Paused(now) {
		t.Error("resume should clear the pause")
	}
}
