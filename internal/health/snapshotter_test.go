package health

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
)

type notFoundError struct{}

func (notFoundError) NotFound()     {}
func (notFoundError) Error() string { return "Error: No such container: c2" }

func summaries(ids ...string) []api.ContainerSummary {
	out := make([]api.ContainerSummary, len(ids))
	for i, id := range ids {
		out[i] = api.ContainerSummary{ID: id, Service: "web", State: StateRunning}
	}
	return out
}

func TestBuildSnapshotTombstonesAVanishedContainer(t *testing.T) {
	inspect := func(_ context.Context, id string) (int, error) {
		if id == "c2" {
			return 0, notFoundError{}
		}
		return 1, nil
	}

	snap, err := buildSnapshot(t.Context(), summaries("c1", "c2", "c3"), inspect)

	if err != nil {
		t.Fatalf("a container vanishing between Ps and Inspect is ordinary during a deploy, want no error: %v", err)
	}
	if len(snap.Containers) != 2 {
		t.Fatalf("Containers = %d, want 2: the vanished one is left out for the watch to judge", len(snap.Containers))
	}
	for _, c := range snap.Containers {
		if c.ID == "c2" {
			t.Errorf("container c2 is gone but still in the snapshot")
		}
		if c.RestartCount != 1 {
			t.Errorf("container %s lost its inspect result", c.ID)
		}
	}
}

func TestBuildSnapshotReportsTheRealErrorNotACancellation(t *testing.T) {
	daemonDown := errors.New("cannot connect to the docker daemon")
	inspect := func(_ context.Context, id string) (int, error) {
		if id == "c1" {
			return 0, daemonDown
		}
		return 0, errors.New("a sibling error that must not mask the first")
	}

	_, err := buildSnapshot(t.Context(), summaries("c1", "c2", "c3"), inspect)

	if err == nil {
		t.Fatal("expected the inspect failure to surface")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the real failure rather than a cancellation from a sibling", err)
	}
}

func TestBuildSnapshotEmptyProject(t *testing.T) {
	snap, err := buildSnapshot(t.Context(), nil, func(context.Context, string) (int, error) {
		t.Fatal("inspect must not be called when there are no containers")
		return 0, nil
	})
	if err != nil || len(snap.Containers) != 0 {
		t.Errorf("buildSnapshot(nil) = %v, %v, want an empty snapshot", snap.Containers, err)
	}
}

func TestBuildSnapshotDoesNotCancelSiblings(t *testing.T) {
	failed := make(chan struct{})
	var sawCancelled atomic.Bool

	inspect := func(ctx context.Context, id string) (int, error) {
		if id == "c1" {
			close(failed)
			return 0, errors.New("cannot connect to the docker daemon")
		}
		<-failed
		for range 50 {
			if ctx.Err() != nil {
				sawCancelled.Store(true)
				return 0, ctx.Err()
			}
			time.Sleep(time.Millisecond)
		}
		return 7, nil
	}

	if _, err := buildSnapshot(t.Context(), summaries("c1", "c2", "c3"), inspect); err == nil {
		t.Fatal("expected the inspect failure to surface")
	}
	if sawCancelled.Load() {
		t.Error("a sibling inspect was cancelled by the first failure, so its real result is lost")
	}
}
