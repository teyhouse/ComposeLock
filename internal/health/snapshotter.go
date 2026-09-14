package health

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/compose/v5/pkg/api"
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

	return buildSnapshot(ctx, summaries, s.restartCount)
}

func buildSnapshot(ctx context.Context, summaries []api.ContainerSummary, inspect func(context.Context, string) (int, error)) (Snapshot, error) {
	containers := make([]ContainerStatus, len(summaries))
	present := make([]bool, len(summaries))

	var g errgroup.Group
	g.SetLimit(maxConcurrentInspects)
	for i, cs := range summaries {
		g.Go(func() error {
			restartCount, err := inspect(ctx, cs.ID)
			if isNotFound(err) {
				return nil
			}
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
			present[i] = true
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Snapshot{}, err
	}

	out := make([]ContainerStatus, 0, len(containers))
	for i, c := range containers {
		if present[i] {
			out = append(out, c)
		}
	}
	return Snapshot{Containers: out, Taken: time.Now()}, nil
}

func isNotFound(err error) bool {
	var notFound interface{ NotFound() }
	return err != nil && errors.As(err, &notFound)
}

func (s *ComposeSnapshotter) restartCount(ctx context.Context, containerID string) (int, error) {
	result, err := s.Service.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return 0, err
	}
	return result.Container.RestartCount, nil
}
