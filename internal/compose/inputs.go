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
	inputs = append(w.paths, declaredPaths(project)...)
	slices.Sort(inputs)
	return slices.Compact(inputs), w.complete
}

func declaredPaths(project *types.Project) []string {
	var paths []string
	add := func(p string) {
		if abs, ok := resolveInput(project.WorkingDir, p); ok {
			paths = append(paths, abs)
		}
	}

	add(".env")
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		svc := project.Services[name]
		for _, ef := range svc.EnvFiles {
			add(ef.Path)
		}
		if svc.Extends != nil {
			add(svc.Extends.File)
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
	return paths
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
		if abs, ok := resolveInput(baseDir, f); ok {
			w.paths = append(w.paths, abs)
		}
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
