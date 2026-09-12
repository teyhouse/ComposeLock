package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/schema"
	"go.yaml.in/yaml/v4"
)

type Stack struct {
	ProjectName string
	Dir         string
	Files       []string
}

func IsComposeFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	default:
		return false
	}
}

var composeTopLevelKeys = map[string]bool{
	"configs":  true,
	"include":  true,
	"name":     true,
	"networks": true,
	"secrets":  true,
	"services": true,
	"version":  true,
	"volumes":  true,
}

func Discover(composeDir, baseProjectName string) ([]Stack, error) {
	entries, err := os.ReadDir(composeDir)
	if err != nil {
		return nil, fmt.Errorf("reading compose_dir %s: %w", composeDir, err)
	}

	var stacks []Stack

	rootFiles, err := composeFilesIn(composeDir, entries)
	if err != nil {
		return nil, err
	}
	if len(rootFiles) > 0 {
		stacks = append(stacks, Stack{ProjectName: baseProjectName, Dir: composeDir, Files: rootFiles})
	}

	for _, e := range entries {
		if isHidden(e.Name()) || !isDir(composeDir, e) {
			continue
		}
		subDir := filepath.Join(composeDir, e.Name())
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", subDir, err)
		}
		files, err := composeFilesIn(subDir, subEntries)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			continue
		}
		stacks = append(stacks, Stack{
			ProjectName: baseProjectName + "-" + sanitizeProjectSuffix(e.Name()),
			Dir:         subDir,
			Files:       files,
		})
	}

	if len(stacks) == 0 {
		return nil, fmt.Errorf("no compose files found in %s (or its immediate subdirectories)", composeDir)
	}

	slices.SortFunc(stacks, func(a, b Stack) int { return strings.Compare(a.ProjectName, b.ProjectName) })

	for i := 1; i < len(stacks); i++ {
		if stacks[i].ProjectName == stacks[i-1].ProjectName {
			return nil, fmt.Errorf("directories %s and %s both map to compose project %q: rename one so the names differ",
				stacks[i-1].Dir, stacks[i].Dir, stacks[i].ProjectName)
		}
	}
	return stacks, nil
}

func composeFilesIn(dir string, entries []os.DirEntry) ([]string, error) {
	var files []string
	for _, e := range entries {
		name := e.Name()
		if isHidden(name) || !IsComposeFile(name) || isDir(dir, e) {
			continue
		}
		path := filepath.Join(dir, name)
		isCompose, err := checkComposeFile(path)
		if err != nil {
			return nil, err
		}
		if isCompose {
			files = append(files, path)
		}
	}
	slices.Sort(files)
	return files, nil
}

func checkComposeFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	var model map[string]any
	if err := yaml.Unmarshal(data, &model); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(model) == 0 || !looksLikeCompose(model) {
		return false, nil
	}
	if err := schema.Validate(model); err != nil {
		return false, fmt.Errorf("invalid compose file %s: %w", path, err)
	}
	return true, nil
}

func looksLikeCompose(model map[string]any) bool {
	if _, ok := model["services"]; ok {
		return true
	}
	if _, ok := model["include"]; ok {
		return true
	}
	for k := range model {
		if !composeTopLevelKeys[k] && !strings.HasPrefix(k, "x-") {
			return false
		}
	}
	return true
}

func isDir(parent string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(parent, e.Name()))
	return err == nil && info.IsDir()
}

func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

func sanitizeProjectSuffix(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
