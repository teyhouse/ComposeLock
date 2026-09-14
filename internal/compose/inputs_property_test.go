package compose

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

const (
	defaultPropertySeed = 1
	defaultPropertyRuns = 1500
)

func propertyConfig(t *testing.T) (uint64, int) {
	t.Helper()
	seed, runs := uint64(defaultPropertySeed), defaultPropertyRuns
	if v := os.Getenv("COMPOSELOCK_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("COMPOSELOCK_PROPERTY_SEED=%q: %v", v, err)
		}
		seed = n
	}
	if v := os.Getenv("COMPOSELOCK_PROPERTY_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("COMPOSELOCK_PROPERTY_RUNS=%q: %v", v, err)
		}
		runs = n
	}
	return seed, runs
}

// refKind is how a generated include or extends reference behaves on disk.
type refKind int

const (
	refPresent refKind = iota
	refMissing
	refInterpolated
	refRemote
	refMalformed
	refKindCount
)

func (r refKind) String() string {
	return [...]string{"present", "missing", "interpolated", "remote", "malformed"}[r]
}

type tree struct {
	dir      string
	project  *types.Project
	refs     []refKind
	tracked  []string // absolute paths of fragments that really exist
	complete bool     // what ProjectInputs must report for this tree
}

func buildTree(t *testing.T, rng *rand.Rand) tree {
	t.Helper()
	dir := t.TempDir()

	var includeLines []string
	tr := tree{dir: dir, complete: true}

	for i := range 1 + rng.IntN(3) {
		kind := refKind(rng.IntN(int(refKindCount)))
		tr.refs = append(tr.refs, kind)
		name := "frag" + strconv.Itoa(i) + ".yaml"
		abs := filepath.Join(dir, name)

		switch kind {
		case refPresent:
			body := "services:\n  s" + strconv.Itoa(i) + ":\n    image: busybox\n"
			if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			includeLines = append(includeLines, "  - "+name)
			tr.tracked = append(tr.tracked, abs)
		case refMissing:
			includeLines = append(includeLines, "  - "+name)
			tr.complete = false
		case refInterpolated:
			includeLines = append(includeLines, "  - ${FRAG_DIR}/"+name)
			tr.complete = false
		case refRemote:
			includeLines = append(includeLines, "  - https://example.com/"+name)
		case refMalformed:
			if err := os.WriteFile(abs, []byte("::: not yaml :::\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			includeLines = append(includeLines, "  - "+name)
			tr.tracked = append(tr.tracked, abs)
			tr.complete = false
		}
	}

	root := filepath.Join(dir, "docker-compose.yml")
	body := "services:\n  web:\n    image: nginx\n"
	if len(includeLines) > 0 {
		body = "include:\n" + strings.Join(includeLines, "\n") + "\n" + body
	}
	if err := os.WriteFile(root, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.tracked = append(tr.tracked, root)

	tr.project = &types.Project{
		Name:         "prop",
		WorkingDir:   dir,
		ComposeFiles: []string{root},
		Services:     types.Services{"web": types.ServiceConfig{Name: "web", Image: "nginx"}},
	}
	return tr
}

func (tr tree) describe() string {
	parts := make([]string, len(tr.refs))
	for i, r := range tr.refs {
		parts[i] = r.String()
	}
	return "includes: [" + strings.Join(parts, " ") + "] dir: " + tr.dir
}

func TestProjectInputsProperties(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))

	for range runs {
		tr := buildTree(t, rng)
		inputs, complete := ProjectInputs(tr.project)

		for _, in := range inputs {
			if !filepath.IsAbs(in) {
				t.Fatalf("input %q is not absolute\n%s", in, tr.describe())
			}
			if in != filepath.Clean(in) {
				t.Fatalf("input %q is not cleaned\n%s", in, tr.describe())
			}
			if strings.Contains(in, "$") {
				t.Fatalf("input %q kept an uninterpolated variable\n%s", in, tr.describe())
			}
			if isRemoteRef(in) {
				t.Fatalf("input %q is a remote reference, which can never be a path in this repository\n%s", in, tr.describe())
			}
		}

		if complete != tr.complete {
			t.Fatalf("complete = %v, want %v: a reference the walker cannot resolve or read must mark the set incomplete\n%s",
				complete, tr.complete, tr.describe())
		}

		for _, want := range tr.tracked {
			if !containsPath(inputs, want) {
				t.Fatalf("input set is missing %q, a file the project really includes\ninputs: %v\n%s",
					want, inputs, tr.describe())
			}
		}

		againInputs, againComplete := ProjectInputs(tr.project)
		if againComplete != complete || len(againInputs) != len(inputs) {
			t.Fatalf("ProjectInputs is not deterministic\n%s", tr.describe())
		}
	}
}

// TestProjectInputsStability pins the property that matters for change detection:
// a file nobody references must never enter the input set, so an unrelated commit
// cannot be mistaken for a relevant one.
func TestProjectInputsStability(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x2545F4914F6CDD1D))

	for range runs {
		tr := buildTree(t, rng)
		before, _ := ProjectInputs(tr.project)

		noise := []string{"README.md", "prometheus.yml", ".gitignore", "notes.txt"}[rng.IntN(4)]
		if err := os.WriteFile(filepath.Join(tr.dir, noise), []byte("unrelated\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		after, _ := ProjectInputs(tr.project)
		if len(after) != len(before) {
			t.Fatalf("adding an unreferenced %s changed the input set from %v to %v\n%s",
				noise, before, after, tr.describe())
		}
		for _, in := range after {
			if filepath.Base(in) == noise {
				t.Fatalf("unreferenced file %s entered the input set\n%s", noise, tr.describe())
			}
		}
	}
}

func TestProjectInputsCycleTerminates(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(a, []byte("include:\n  - b.yaml\nservices:\n  x:\n    image: busybox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("include:\n  - a.yaml\nservices:\n  y:\n    image: busybox\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	project := &types.Project{Name: "cycle", WorkingDir: dir, ComposeFiles: []string{a}}
	inputs, _ := ProjectInputs(project)

	if !containsPath(inputs, a) || !containsPath(inputs, b) {
		t.Errorf("inputs = %v, want both sides of the cycle tracked", inputs)
	}
	seen := map[string]int{}
	for _, in := range inputs {
		seen[in]++
		if seen[in] > 1 {
			t.Errorf("input %q appears %d times: a cycle must not duplicate entries", in, seen[in])
		}
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func FuzzProjectInputs(f *testing.F) {
	f.Add("services:\n  web:\n    image: nginx\n")
	f.Add("include:\n  - frag.yaml\nservices:\n  web:\n    image: nginx\n")
	f.Add("include:\n  - ${DIR}/frag.yaml\n")
	f.Add("include:\n  - path: [a.yaml, b.yaml]\n    project_directory: sub\n    env_file: app.env\n")
	f.Add("services:\n  web:\n    extends:\n      file: base.yaml\n      service: b\n")
	f.Add("include: not-a-list\n")
	f.Add("")

	// One directory reused for every input: a fresh t.TempDir() per execution
	// creates tens of thousands of directories and stalls the fuzzer outright.
	dir := f.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")

	f.Fuzz(func(t *testing.T, body string) {
		if err := os.WriteFile(root, []byte(body), 0o600); err != nil {
			t.Skip()
		}

		project := &types.Project{Name: "fuzz", WorkingDir: dir, ComposeFiles: []string{root}}
		inputs, complete := ProjectInputs(project)

		for _, in := range inputs {
			if !filepath.IsAbs(in) {
				t.Errorf("input %q is not absolute (body %q)", in, body)
			}
			if complete && strings.Contains(in, "$") {
				t.Errorf("input %q kept a variable while the set claims to be complete (body %q)", in, body)
			}
			if complete && isRemoteRef(in) {
				t.Errorf("remote reference %q in a set claiming to be complete (body %q)", in, body)
			}
		}
		if complete && !containsPath(inputs, root) {
			t.Errorf("the compose file itself is missing from a complete input set (body %q)", body)
		}
	})
}

// declaredField builds a project whose one declared path is value, so a property
// can assert how an unresolvable declaration affects completeness.
func declaredField(dir, field, value string) *types.Project {
	svc := types.ServiceConfig{Name: "web", Image: "nginx"}
	project := &types.Project{Name: "p", WorkingDir: dir, ComposeFiles: nil}
	switch field {
	case "env_file":
		svc.EnvFiles = []types.EnvFile{{Path: value, Required: true}}
	case "extends":
		svc.Extends = &types.ExtendsConfig{File: value, Service: "base"}
	case "build_context":
		svc.Build = &types.BuildConfig{Context: value}
	case "dockerfile":
		svc.Build = &types.BuildConfig{Context: "src", Dockerfile: value}
	case "config":
		project.Configs = types.Configs{"c": types.ConfigObjConfig{File: value}}
	case "secret":
		project.Secrets = types.Secrets{"s": types.SecretConfig{File: value}}
	}
	project.Services = types.Services{"web": svc}
	return project
}

// TestDeclaredInputsCompletenessProperties pins the rule that a declared path the
// walker cannot resolve makes the set incomplete, while a value that declares
// nothing, or one that can never be a path here, does not.
func TestDeclaredInputsCompletenessProperties(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x14057B7EF767814F))
	fields := []string{"env_file", "extends", "build_context", "dockerfile", "config", "secret"}

	for range runs {
		dir := t.TempDir()
		field := fields[rng.IntN(len(fields))]

		unresolvable := []string{"${CFG}/app.env", "$CFG/app.env", "prefix${VAR}suffix"}[rng.IntN(3)]
		if _, complete := ProjectInputs(declaredField(dir, field, unresolvable)); complete {
			t.Fatalf("%s = %q was dropped while the set still claims to be complete: a caller cannot tell the input was never weighed",
				field, unresolvable)
		}

		droppable := []string{"", "https://example.com/x.yaml", "git@github.com:o/r.git"}[rng.IntN(3)]
		if field == "dockerfile" && droppable == "" {
			continue // an absent dockerfile is defaulted by the loader, not declared here
		}
		if _, complete := ProjectInputs(declaredField(dir, field, droppable)); !complete {
			t.Fatalf("%s = %q must not make the set incomplete: it declares no path in this repository",
				field, droppable)
		}
	}
}

func TestDockerfileResolvesAgainstTheBuildContext(t *testing.T) {
	dir := t.TempDir()
	project := declaredField(dir, "dockerfile", "Dockerfile.alt")

	inputs, complete := ProjectInputs(project)

	want := filepath.Join(dir, "src", "Dockerfile.alt")
	phantom := filepath.Join(dir, "Dockerfile.alt")
	if !containsPath(inputs, want) {
		t.Errorf("inputs = %v, want %q: dockerfile is relative to the build context", inputs, want)
	}
	if containsPath(inputs, phantom) {
		t.Errorf("inputs = %v, must not track %q: that path is a phantom and the real dockerfile would go unwatched", inputs, phantom)
	}
	if !complete {
		t.Error("a resolvable context and dockerfile must leave the set complete")
	}
}
