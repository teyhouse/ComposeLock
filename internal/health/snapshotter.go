package health

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	"github.com/teyhouse/ComposeLock/internal/compose"
)

const maxConcurrentInspects = 16

type ComposeSnapshotter struct {
	Service *compose.Service
	Timeout time.Duration
}

func (s *ComposeSnapshotter) Snapshot(ctx context.Context, projectName string) (Snapshot, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	summaries, err := s.Service.Ps(ctx, projectName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("listing containers: %w", err)
	}

	containers := make([]ContainerStatus, len(summaries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentInspects)
	for i, cs := range summaries {
		g.Go(func() error {
			restartCount, err := s.restartCount(gctx, cs.ID)
			if err != nil {
				return fmt.Errorf("inspecting container %s: %w", cs.ID, err)
			}
			containers[i] = ContainerStatus{
				ID:           cs.ID,
				Service:      cs.Service,
				State:        string(cs.State),
				Health:       string(cs.Health),
				RestartCount: restartCount,
				ExitCode:     cs.ExitCode,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Snapshot{}, err
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
