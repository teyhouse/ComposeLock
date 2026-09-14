package compose

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/client"
)

const maxIncludeDepth = 16

func ProjectInputs(project *types.Project) (inputs []string, complete bool) {
	w := &inputWalker{seen: make(map[string]bool), complete: true}
	for _, f := range project.ComposeFiles {
		w.visit(f, project.WorkingDir, project.WorkingDir, 0)
	}
	declared, declaredComplete := declaredPaths(project)
	inputs = append(w.paths, declared...)
	slices.Sort(inputs)
	return slices.Compact(inputs), w.complete && declaredComplete
}

func declaredPaths(project *types.Project) (paths []string, complete bool) {
	complete = true
	dir := project.WorkingDir

	// An empty value declares nothing, and a remote reference can never be a path
	// in this repository. Anything else that will not resolve, such as a path
	// built from a variable, is an input we cannot see, so the set is incomplete.
	add := func(base, p string) {
		if p == "" || isRemoteRef(p) {
			return
		}
		abs, ok := resolveInput(base, p)
		if !ok {
			complete = false
			return
		}
		paths = append(paths, abs)
	}
	// The implicit .env is inferred rather than declared, so a project with no
	// working directory to resolve it against is not treated as a gap.
	if abs, ok := resolveInput(dir, ".env"); ok {
		paths = append(paths, abs)
	}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		svc := project.Services[name]
		for _, ef := range svc.EnvFiles {
			add(dir, ef.Path)
		}
		if svc.Extends != nil {
			add(dir, svc.Extends.File)
		}
		if svc.Build != nil {
			add(dir, svc.Build.Context)
			// dockerfile is relative to the build context, not to the project. A
			// remote context holds its dockerfile remotely, so it is not a gap.
			if !isRemoteRef(svc.Build.Context) {
				if ctx, ok := buildContextDir(dir, svc.Build.Context); ok {
					add(ctx, svc.Build.Dockerfile)
				} else if svc.Build.Dockerfile != "" {
					complete = false
				}
			}
			for _, extra := range svc.Build.AdditionalContexts {
				add(dir, extra)
			}
		}
	}
	for _, c := range project.Configs {
		add(dir, c.File)
	}
	for _, sec := range project.Secrets {
		add(dir, sec.File)
	}
	return paths, complete
}

// buildContextDir reports the directory a service's dockerfile resolves against.
func buildContextDir(workingDir, context string) (string, bool) {
	if context == "" {
		return workingDir, workingDir != ""
	}
	return resolveInput(workingDir, context)
}

type inputWalker struct {
	paths    []string
	seen     map[string]bool
	complete bool
}

func (w *inputWalker) visit(ref, refBase, contentsDir string, depth int) {
	if isRemoteRef(ref) {
		return
	}
	abs, ok := resolveInput(refBase, ref)
	if !ok {
		w.complete = false
		return
	}
	if w.seen[abs] {
		return
	}
	w.seen[abs] = true
	w.paths = append(w.paths, abs)

	if depth >= maxIncludeDepth {
		w.complete = false
		return
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		w.complete = false
		return
	}
	docs, err := yamlMappings(data)
	if err != nil || len(docs) == 0 {
		w.complete = false
		return
	}
	w.walkIncludes(docs[0]["include"], contentsDir, depth)
	w.walkExtends(docs[0]["services"], contentsDir, depth)
}

func (w *inputWalker) walkIncludes(node any, baseDir string, depth int) {
	entries, ok := node.([]any)
	if node != nil && !ok {
		w.complete = false
		return
	}
	for _, entry := range entries {
		switch e := entry.(type) {
		case string:
			w.walkInclude([]string{e}, "", nil, baseDir, depth)
		case map[string]any:
			paths, pathsOK := stringList(e["path"])
			projectDir, dirOK := optionalString(e["project_directory"])
			envFiles, envOK := stringList(e["env_file"])
			if !pathsOK || !dirOK || !envOK {
				w.complete = false
				continue
			}
			w.walkInclude(paths, projectDir, envFiles, baseDir, depth)
		default:
			w.complete = false
		}
	}
}

func (w *inputWalker) walkInclude(paths []string, projectDir string, envFiles []string, baseDir string, depth int) {
	if len(paths) == 0 {
		w.complete = false
		return
	}
	includeDir := ""
	switch {
	case projectDir != "":
		abs, ok := resolveInput(baseDir, projectDir)
		if !ok {
			w.complete = false
			return
		}
		includeDir = abs
	default:
		if abs, ok := resolveInput(baseDir, paths[0]); ok {
			includeDir = filepath.Dir(abs)
		}
	}

	for _, f := range envFiles {
		abs, ok := resolveInput(baseDir, f)
		if !ok {
			w.complete = false
			continue
		}
		w.paths = append(w.paths, abs)
	}
	if len(envFiles) == 0 && includeDir != "" {
		if abs, ok := resolveInput(includeDir, ".env"); ok {
			w.paths = append(w.paths, abs)
		}
	}
	for _, p := range paths {
		w.visit(p, baseDir, includeDir, depth+1)
	}
}

func (w *inputWalker) walkExtends(node any, baseDir string, depth int) {
	services, ok := node.(map[string]any)
	if node != nil && !ok {
		w.complete = false
		return
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		extends, ok := svc["extends"].(map[string]any)
		if !ok {
			continue
		}
		file, fileOK := optionalString(extends["file"])
		if !fileOK {
			w.complete = false
			continue
		}
		if file == "" {
			continue
		}
		if abs, ok := resolveInput(baseDir, file); ok {
			w.visit(abs, baseDir, filepath.Dir(abs), depth+1)
			continue
		}
		w.complete = false
	}
}

func resolveInput(baseDir, p string) (string, bool) {
	if p == "" || strings.Contains(p, "$") || isRemoteRef(p) {
		return "", false
	}
	if !filepath.IsAbs(p) {
		if baseDir == "" {
			return "", false
		}
		p = filepath.Join(baseDir, p)
	}
	return filepath.Clean(p), true
}

func stringList(node any) ([]string, bool) {
	switch v := node.(type) {
	case nil:
		return nil, true
	case string:
		return []string{v}, true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

func optionalString(node any) (string, bool) {
	switch v := node.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	default:
		return "", false
	}
}

func isRemoteRef(p string) bool {
	return strings.Contains(p, "://") || strings.HasPrefix(p, "git@")
}

func IsInfraError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if client.IsErrConnectionFailed(err) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
