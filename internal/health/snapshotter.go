package health

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"github.com/teyhouse/ComposeLock/internal/compose"
)

type ComposeSnapshotter struct {
	Service *compose.Service
}

func (s *ComposeSnapshotter) Snapshot(ctx context.Context, projectName string) (Snapshot, error) {
	summaries, err := s.Service.Ps(ctx, projectName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("listing containers: %w", err)
	}

	containers := make([]ContainerStatus, len(summaries))
	for i, cs := range summaries {
		restartCount, err := s.restartCount(ctx, cs.ID)
		if err != nil {
			return Snapshot{}, fmt.Errorf("inspecting container %s: %w", cs.ID, err)
		}
		containers[i] = ContainerStatus{
			ID:           cs.ID,
			Service:      cs.Service,
			State:        string(cs.State),
			Health:       string(cs.Health),
			RestartCount: restartCount,
		}
	}

	return Snapshot{Containers: containers, Taken: time.Now()}, nil
}

func (s *ComposeSnapshotter) restartCount(ctx context.Context, containerID string) (int, error) {
	result, err := s.Service.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return 0, err
	}
	return result.Container.RestartCount, nil
}
