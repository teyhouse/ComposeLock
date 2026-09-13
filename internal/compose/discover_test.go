package compose

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const validComposeYAML = `
services:
  web:
    image: nginx:alpine
`

const invalidComposeYAML = `
services: web
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stackNames(stacks []Stack) []string {
	names := make([]string, len(stacks))
	for i, s := range stacks {
		names[i] = s.ProjectName
	}
	sort.Strings(names)
	return names
}

func TestDiscoverRootAndOneSubdir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "service-a.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "service-b.yml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "db", "docker-compose.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got, want := stackNames(stacks), []string{"myproject", "myproject-db"}; !equalStrings(got, want) {
		t.Fatalf("stack names = %v, want %v", got, want)
	}

	for _, s := range stacks {
		if s.ProjectName == "myproject" {
			if len(s.Files) != 2 {
				t.Fatalf("root stack files = %v, want 2 files", s.Files)
			}
			if !strings.HasSuffix(s.Files[0], "service-a.yaml") || !strings.HasSuffix(s.Files[1], "service-b.yml") {
				t.Fatalf("root stack files not sorted: %v", s.Files)
			}
		}
		if s.ProjectName == "myproject-db" && len(s.Files) != 1 {
			t.Fatalf("db stack files = %v, want 1 file", s.Files)
		}
	}
}

func TestDiscoverIgnoresDeeperNesting(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "db", "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "db", "nested", "should-be-ignored.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected exactly one stack with one file, got %+v", stacks)
	}
}

func TestDiscoverIgnoresNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "service-a.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "README.md"), "not a compose file")

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected exactly one file, got %+v", stacks)
	}
}

func TestDiscoverInvalidFileFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "service-a.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "broken.yaml"), invalidComposeYAML)

	_, err := Discover(dir, "myproject", nil)
	if err == nil {
		t.Fatal("expected an error for the invalid compose file")
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("error should name the broken file, got: %v", err)
	}
}

func TestDiscoverInvalidFileInSubdirFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "db", "broken.yaml"), invalidComposeYAML)

	_, err := Discover(dir, "myproject", nil)
	if err == nil {
		t.Fatal("expected an error for the invalid compose file")
	}
}

func TestDiscoverMissingDirectory(t *testing.T) {
	_, err := Discover(filepath.Join(t.TempDir(), "does-not-exist"), "myproject", nil)
	if err == nil {
		t.Fatal("expected an error for a missing compose_dir")
	}
}

func TestDiscoverNoComposeFilesAnywhere(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "nothing here")

	_, err := Discover(dir, "myproject", nil)
	if err == nil {
		t.Fatal("expected an error when no compose files are found")
	}
}

func TestDiscoverSanitizesSubdirNameForProjectName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "My DB!", "docker-compose.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 {
		t.Fatalf("expected one stack, got %+v", stacks)
	}
	if stacks[0].ProjectName != "myproject-my-db-" {
		t.Fatalf("ProjectName = %q, want %q", stacks[0].ProjectName, "myproject-my-db-")
	}
}

func TestIsComposeFile(t *testing.T) {
	cases := map[string]bool{
		"docker-compose.yaml": true,
		"docker-compose.yml":  true,
		"Service.YAML":        true,
		"README.md":           false,
		"compose.json":        false,
	}
	for name, want := range cases {
		if got := IsComposeFile(name); got != want {
			t.Errorf("IsComposeFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiscoverSetsStackDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "service-a.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "db", "docker-compose.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]string{"myproject": dir, "myproject-db": filepath.Join(dir, "db")}
	for _, s := range stacks {
		if s.Dir != want[s.ProjectName] {
			t.Errorf("%s Dir = %q, want %q", s.ProjectName, s.Dir, want[s.ProjectName])
		}
	}
}

func TestDiscoverSkipsNonComposeYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "prometheus.yml"), "global:\n  scrape_interval: 15s\nscrape_configs: []\n")
	writeFile(t, filepath.Join(dir, "dependabot.yml"), "version: 2\nupdates: []\n")
	writeFile(t, filepath.Join(dir, "manifest.yaml"), "apiVersion: v1\nkind: ConfigMap\n")
	writeFile(t, filepath.Join(dir, "empty.yaml"), "")
	writeFile(t, filepath.Join(dir, "comment-only.yaml"), "# nothing to see\n")

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected only the compose file, got %+v", stacks)
	}
	if !strings.HasSuffix(stacks[0].Files[0], "docker-compose.yaml") {
		t.Errorf("discovered %q, want docker-compose.yaml", stacks[0].Files[0])
	}
}

func TestDiscoverKeepsComposeFragments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a-networks.yaml"), "networks:\n  shared:\n    external: true\n")
	writeFile(t, filepath.Join(dir, "b-services.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 2 {
		t.Fatalf("expected the fragment to be merged in, got %+v", stacks)
	}
}

func TestDiscoverStillFailsOnBrokenComposeFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "broken.yaml"), invalidComposeYAML)

	_, err := Discover(dir, "myproject", nil)
	if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("err = %v, want it to name broken.yaml", err)
	}
}

func TestDiscoverFailsOnUnparsableYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "garbage.yaml"), "services:\n  web:\n   - bad\n  \tindent\n")

	_, err := Discover(dir, "myproject", nil)
	if err == nil || !strings.Contains(err.Error(), "garbage.yaml") {
		t.Fatalf("err = %v, want it to name garbage.yaml", err)
	}
}

func TestDiscoverSkipsHiddenEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, ".github", "dependabot.yml"), "version: 2\nupdates: []\n")
	writeFile(t, filepath.Join(dir, ".hidden.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || stacks[0].ProjectName != "myproject" || len(stacks[0].Files) != 1 {
		t.Fatalf("expected only the visible root stack, got %+v", stacks)
	}
}

func TestDiscoverFollowsSymlinkedStackDir(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "docker-compose.yaml"), validComposeYAML)
	if err := os.Symlink(target, filepath.Join(dir, "db")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || stacks[0].ProjectName != "myproject-db" {
		t.Fatalf("expected the symlinked directory to be scanned, got %+v", stacks)
	}
}

func TestDiscoverRejectsCollidingStackNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "my db", "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "my-db", "docker-compose.yaml"), validComposeYAML)

	_, err := Discover(dir, "myproject", nil)
	if err == nil || !strings.Contains(err.Error(), "myproject-my-db") {
		t.Fatalf("err = %v, want a collision error naming the shared project name", err)
	}
}

func TestDiscoverPutsOverrideFileLast(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "docker-compose.override.yml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 2 {
		t.Fatalf("expected one stack with two files, got %+v", stacks)
	}
	if base := filepath.Base(stacks[0].Files[0]); base != "docker-compose.yml" {
		t.Errorf("Files[0] = %q, want the base file first", base)
	}
	if base := filepath.Base(stacks[0].Files[1]); base != "docker-compose.override.yml" {
		t.Errorf("Files[1] = %q, want the override file last so it wins the merge", base)
	}
}

func TestDiscoverSkipsNonMappingYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "hosts.yaml"), "- one\n- two\n")

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v, want a top-level list to be skipped, not to fail the sync", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected only the compose file, got %+v", stacks)
	}
}

func TestDiscoverAcceptsModelsOnlyFragment(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "models.yaml"), "models:\n  llm:\n    model: ai/smol\n")

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 2 {
		t.Fatalf("expected models.yaml to be merged, got %+v", stacks)
	}
}

func TestDiscoverSkipsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	if err := os.Symlink(filepath.Join(dir, "gone.yaml"), filepath.Join(dir, "broken.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v, want a dangling symlink to be skipped", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected only the real compose file, got %+v", stacks)
	}
}

func TestDiscoverMissingDirReportsErrNoStacks(t *testing.T) {
	_, err := Discover(filepath.Join(t.TempDir(), "absent"), "myproject", nil)
	if !errors.Is(err, ErrNoStacks) {
		t.Fatalf("err = %v, want it to wrap ErrNoStacks", err)
	}
}

func TestDiscoverInvalidComposeFileStillFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "broken.yaml"), "services: not-a-mapping\n")

	_, err := Discover(dir, "myproject", nil)
	if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("err = %v, want a loud failure naming broken.yaml", err)
	}
	if errors.Is(err, ErrNoStacks) {
		t.Error("a broken compose file must not look like an empty compose_dir")
	}
}

func TestDiscoverCachedStacksAreNotAliased(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)

	first, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	first[0].Files[0] = "tampered"

	second, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if second[0].Files[0] == "tampered" {
		t.Error("mutating a returned stack corrupted the cache")
	}
}

func TestDiscoverErrorsOnAMappingFollowedByAScalarDocument(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML+"---\njust a string\n")

	_, err := Discover(dir, "myproject", nil)
	if err == nil {
		t.Fatal("Discover() = nil, want a stray trailing document to be an error, not a silent skip")
	}
	if errors.Is(err, ErrNoStacks) {
		t.Errorf("err = %v, want a parse error rather than the stack vanishing", err)
	}
}

func TestDiscoverStillSkipsNonComposeYAMLStartingWithAList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "playbook.yaml"), "- hosts: all\n  tasks: []\n")
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(stacks) != 1 || len(stacks[0].Files) != 1 {
		t.Fatalf("expected only the compose file to be picked up, got %+v", stacks)
	}
}

func TestDiscoverSkipsADirectoryOfFragmentsWithNoWorkloads(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), "version: \"3\"\nnetworks:\n  shared:\n    external: true\n")

	_, err := Discover(dir, "myproject", nil)
	if !errors.Is(err, ErrNoStacks) {
		t.Errorf("err = %v, want ErrNoStacks: a stack with no services would report NO CONTAINERS", err)
	}
}

func TestDiscoverSkipsAFragmentOnlySubdirButKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yaml"), validComposeYAML)
	writeFile(t, filepath.Join(dir, "shared", "networks.yaml"), "networks:\n  shared:\n    external: true\n")

	stacks, err := Discover(dir, "myproject", nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := stackNames(stacks); len(got) != 1 || got[0] != "myproject" {
		t.Errorf("stacks = %v, want only [myproject]", got)
	}
}
