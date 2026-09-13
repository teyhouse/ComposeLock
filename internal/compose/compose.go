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
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
)

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
	ctx, cancel := s.withTimeout(ctx, s.upTimeout)
	defer cancel()
	if err := s.compose.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{RemoveOrphans: true},
		Start:  api.StartOptions{Project: project},
	}); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	return nil
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

func ProjectPaths(project *types.Project) []string {
	var paths []string
	add := func(p string) {
		if p == "" || isRemoteRef(p) {
			return
		}
		if !filepath.IsAbs(p) {
			if project.WorkingDir == "" {
				return
			}
			p = filepath.Join(project.WorkingDir, p)
		}
		paths = append(paths, filepath.Clean(p))
	}

	for _, f := range project.ComposeFiles {
		add(f)
	}
	add(".env")
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		svc := project.Services[name]
		for _, ef := range svc.EnvFiles {
			add(ef.Path)
		}
		if svc.Build != nil {
			add(svc.Build.Context)
			add(svc.Build.Dockerfile)
			for _, ctx := range svc.Build.AdditionalContexts {
				add(ctx)
			}
		}
	}
	for _, c := range project.Configs {
		add(c.File)
	}
	for _, sec := range project.Secrets {
		add(sec.File)
	}

	slices.Sort(paths)
	return slices.Compact(paths)
}

func isRemoteRef(p string) bool {
	return strings.Contains(p, "://") || strings.HasPrefix(p, "git@")
}
