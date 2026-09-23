package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
)

var errRegistryUnavailable = errors.New("registry unavailable")

type Service struct {
	compose   api.Compose
	cli       command.Cli
	timeout   time.Duration
	upTimeout time.Duration
}

func New(timeout, upTimeout time.Duration) (*Service, error) {
	// The SDK's own progress output defaults to os.Stdout/os.Stderr, which
	// would interleave with our JSON log stream and break log parsing.
	dockerCLI, err := command.NewDockerCli(
		command.WithOutputStream(io.Discard),
		command.WithErrorStream(io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("creating docker cli: %w", err)
	}
	if err := dockerCLI.Initialize(&flags.ClientOptions{}); err != nil {
		return nil, fmt.Errorf("initializing docker cli: %w", err)
	}

	svc, err := compose.NewComposeService(dockerCLI)
	if err != nil {
		return nil, fmt.Errorf("creating compose service: %w", err)
	}

	return &Service{compose: svc, cli: dockerCLI, timeout: timeout, upTimeout: upTimeout}, nil
}

func (s *Service) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func (s *Service) LoadProject(ctx context.Context, composeFiles []string, projectName string) (*types.Project, error) {
	ctx, cancel := s.withTimeout(ctx, s.timeout)
	defer cancel()
	project, err := s.compose.LoadProject(ctx, api.ProjectLoadOptions{
		ConfigPaths: composeFiles,
		ProjectName: projectName,
	})
	if err != nil {
		return nil, fmt.Errorf("loading compose project %s: %w", strings.Join(composeFiles, ", "), err)
	}
	return project, nil
}

func (s *Service) Up(ctx context.Context, project *types.Project) error {
	upCtx, cancel := s.withTimeout(ctx, s.upTimeout)
	defer cancel()
	forceBuild(project)
	if err := s.compose.Up(upCtx, project, api.UpOptions{
		Create: api.CreateOptions{
			RemoveOrphans: true,
			Build:         &api.BuildOptions{Deps: true},
		},
		Start: api.StartOptions{Project: project},
	}); err != nil {
		return fmt.Errorf("compose up: %w", ownDeadline(ctx, upCtx, err, s.upTimeout))
	}
	return nil
}

func (s *Service) Pull(ctx context.Context, project *types.Project) error {
	pullCtx, cancel := s.withTimeout(ctx, s.upTimeout)
	defer cancel()
	services, err := s.imagesToPull(pullCtx, project)
	if err != nil {
		return fmt.Errorf("compose pull: %w", err)
	}
	if len(services) == 0 {
		return nil
	}
	subset := *project
	subset.Services = services
	if err := s.compose.Pull(pullCtx, &subset, api.PullOptions{Quiet: true}); err != nil {
		return fmt.Errorf("compose pull: %w", classifyPullError(ownDeadline(ctx, pullCtx, err, s.upTimeout)))
	}
	return nil
}

func classifyPullError(err error) error {
	if cerrdefs.IsNotFound(err) || cerrdefs.IsInvalidArgument(err) || cerrdefs.IsUnauthorized(err) || cerrdefs.IsPermissionDenied(err) {
		return err
	}
	return fmt.Errorf("%w: %w", errRegistryUnavailable, err)
}

func (s *Service) imagesToPull(ctx context.Context, project *types.Project) (types.Services, error) {
	services := types.Services{}
	for name, svc := range project.Services {
		if svc.Image == "" || svc.Build != nil || svc.Provider != nil {
			continue
		}
		policy, _, err := svc.GetPullPolicy()
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		switch policy {
		case types.PullPolicyAlways:
		case types.PullPolicyMissing, types.PullPolicyIfNotPresent:
			present, err := s.imagePresent(ctx, svc.Image)
			if err != nil {
				return nil, err
			}
			if present {
				continue
			}
		default:
			continue
		}
		services[name] = svc
	}
	return services, nil
}

func (s *Service) imagePresent(ctx context.Context, image string) (bool, error) {
	_, err := s.cli.Client().ImageInspect(ctx, image)
	switch {
	case err == nil:
		return true, nil
	case cerrdefs.IsNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("inspecting image %s: %w", image, err)
}

func ownDeadline(caller, inner context.Context, err error, budget time.Duration) error {
	if caller.Err() != nil || !errors.Is(inner.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("timed out after %s: %v", budget, err)
}

func forceBuild(project *types.Project) {
	for name, svc := range project.Services {
		if svc.Build == nil {
			continue
		}
		svc.PullPolicy = types.PullPolicyBuild
		project.Services[name] = svc
	}
}

func (s *Service) Down(ctx context.Context, projectName string) error {
	ctx, cancel := s.withTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.compose.Down(ctx, projectName, api.DownOptions{RemoveOrphans: true}); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	return nil
}

// Ps lists containers for the project. All: true is required — the
// zero-value default matches `docker compose ps` and excludes
// stopped/exited containers, which would make the health watcher's "did a
// container exit" check silently never fire.
func (s *Service) Ps(ctx context.Context, projectName string) ([]api.ContainerSummary, error) {
	ctx, cancel := s.withTimeout(ctx, s.timeout)
	defer cancel()
	containers, err := s.compose.Ps(ctx, projectName, api.PsOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("compose ps: %w", err)
	}
	return containers, nil
}

func (s *Service) Client() client.APIClient {
	return s.cli.Client()
}

func (s *Service) Close() error {
	return s.cli.Client().Close()
}

// CheckEnvFiles verifies referenced env_file paths exist. Existence check
// only — it never opens, reads, or logs their contents.
func CheckEnvFiles(project *types.Project) error {
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		for _, ef := range project.Services[name].EnvFiles {
			if !ef.Required {
				continue
			}
			path := ef.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(project.WorkingDir, path)
			}
			info, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("env_file not found: %s (referenced by service %q)", path, name)
				}
				return fmt.Errorf("checking env_file %s (referenced by service %q): %w", path, name, err)
			}
			if info.IsDir() {
				return fmt.Errorf("env_file is a directory: %s (referenced by service %q)", path, name)
			}
		}
	}
	return nil
}
