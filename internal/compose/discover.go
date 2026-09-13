package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/schema"
	"go.yaml.in/yaml/v4"
)

var ErrNoStacks = errors.New("no compose stacks found")

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

func IsHiddenPath(rel string) bool {
	for _, seg := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if isHidden(seg) {
			return true
		}
	}
	return false
}

var composeTopLevelKeys = map[string]bool{
	"configs":  true,
	"include":  true,
	"models":   true,
	"name":     true,
	"networks": true,
	"secrets":  true,
	"services": true,
	"version":  true,
	"volumes":  true,
}

var composeBaseNames = map[string]bool{
	"compose.yaml":        true,
	"compose.yml":         true,
	"docker-compose.yaml": true,
	"docker-compose.yml":  true,
}

type discoverCache struct {
	mu          sync.Mutex
	key         string
	fingerprint string
	stacks      []Stack
	err         error
}

var cache discoverCache

func Discover(composeDir, baseProjectName string) ([]Stack, error) {
	key := composeDir + "\x00" + baseProjectName

	fp, fpErr := fingerprint(composeDir)
	if fpErr == nil {
		cache.mu.Lock()
		hit := cache.key == key && cache.fingerprint == fp
		stacks, err := cache.stacks, cache.err
		cache.mu.Unlock()
		if hit {
			return cloneStacks(stacks), err
		}
	}

	stacks, err := discover(composeDir, baseProjectName)

	if fpErr == nil {
		cache.mu.Lock()
		cache.key, cache.fingerprint, cache.stacks, cache.err = key, fp, stacks, err
		cache.mu.Unlock()
	}
	return cloneStacks(stacks), err
}

func cloneStacks(stacks []Stack) []Stack {
	out := slices.Clone(stacks)
	for i := range out {
		out[i].Files = slices.Clone(out[i].Files)
	}
	return out
}

func fingerprint(composeDir string) (string, error) {
	entries, err := os.ReadDir(composeDir)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	if err := hashComposeFiles(h, composeDir, entries); err != nil {
		return "", err
	}
	for _, e := range entries {
		subDir, ok := subDirOf(composeDir, e)
		if !ok {
			continue
		}
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "d\x00%s\x00", subDir)
		if err := hashComposeFiles(h, subDir, subEntries); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashComposeFiles(h hash.Hash, dir string, entries []os.DirEntry) error {
	for _, e := range entries {
		if !isComposeCandidate(dir, e) {
			continue
		}
		name := e.Name()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "f\x00%s\x00%d\x00", name, len(data))
		h.Write(data)
	}
	return nil
}

func discover(composeDir, baseProjectName string) ([]Stack, error) {
	entries, err := os.ReadDir(composeDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("reading compose_dir %s: %w: %w", composeDir, ErrNoStacks, err)
		}
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
		subDir, ok := subDirOf(composeDir, e)
		if !ok {
			continue
		}
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
		return nil, fmt.Errorf("%w in %s (or its immediate subdirectories)", ErrNoStacks, composeDir)
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
		if !isComposeCandidate(dir, e) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		isCompose, err := checkComposeFile(path)
		if err != nil {
			return nil, err
		}
		if isCompose {
			files = append(files, path)
		}
	}
	sortByMergeOrder(files)
	return files, nil
}

func sortByMergeOrder(files []string) {
	slices.SortFunc(files, func(a, b string) int {
		if ra, rb := mergeRank(filepath.Base(a)), mergeRank(filepath.Base(b)); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
}

func mergeRank(name string) int {
	name = strings.ToLower(name)
	switch {
	case composeBaseNames[name]:
		return 0
	case strings.HasSuffix(strings.TrimSuffix(name, filepath.Ext(name)), ".override"):
		return 2
	default:
		return 1
	}
}

func checkComposeFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	docs, err := yamlMappings(data)
	if err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(docs) == 0 || !looksLikeCompose(docs[0]) {
		return false, nil
	}
	if len(docs) > 1 {
		return false, fmt.Errorf("multi-document compose file %s: only the first document would be applied", path)
	}
	if err := schema.Validate(docs[0]); err != nil {
		return false, fmt.Errorf("invalid compose file %s: %w", path, err)
	}
	return true, nil
}

func yamlMappings(data []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if doc == nil {
			continue
		}
		mapping, ok := doc.(map[string]any)
		if !ok {
			return nil, nil
		}
		docs = append(docs, mapping)
	}
}

func looksLikeCompose(model map[string]any) bool {
	if len(model) == 0 {
		return false
	}
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

func isComposeCandidate(parent string, e os.DirEntry) bool {
	name := e.Name()
	if isHidden(name) || !IsComposeFile(name) {
		return false
	}
	isDir, ok := entryIsDir(parent, e)
	return ok && !isDir
}

func subDirOf(parent string, e os.DirEntry) (string, bool) {
	if isHidden(e.Name()) {
		return "", false
	}
	if isDir, ok := entryIsDir(parent, e); !ok || !isDir {
		return "", false
	}
	return filepath.Join(parent, e.Name()), true
}

func entryIsDir(parent string, e os.DirEntry) (bool, bool) {
	if e.Type()&os.ModeSymlink == 0 {
		return e.IsDir(), true
	}
	info, err := os.Stat(filepath.Join(parent, e.Name()))
	if err != nil {
		return false, false
	}
	return info.IsDir(), true
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
