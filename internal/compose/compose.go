package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
)

type Service struct {
	compose api.Compose
	cli     command.Cli
}

func New() (*Service, error) {
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

	return &Service{compose: svc, cli: dockerCLI}, nil
}

func (s *Service) LoadProject(ctx context.Context, composeFiles []string, projectName string) (*types.Project, error) {
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
	if err := s.compose.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{RemoveOrphans: true},
		Start:  api.StartOptions{Project: project},
	}); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	return nil
}

func (s *Service) Down(ctx context.Context, projectName string) error {
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
	containers, err := s.compose.Ps(ctx, projectName, api.PsOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("compose ps: %w", err)
	}
	return containers, nil
}

func (s *Service) Client() client.APIClient {
	return s.cli.Client()
}

// CheckEnvFiles verifies referenced env_file paths exist. Existence check
// only — it never opens, reads, or logs their contents.
func CheckEnvFiles(project *types.Project) error {
	for name, svc := range project.Services {
		for _, ef := range svc.EnvFiles {
			path := ef.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(project.WorkingDir, path)
			}
			if _, err := os.Stat(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("env_file not found: %s (referenced by service %q)", path, name)
				}
				return fmt.Errorf("checking env_file %s (referenced by service %q): %w", path, name, err)
			}
		}
	}
	return nil
}
