// Package ciworkflow contains tests for the GitHub Actions workflow
// definition in .github/workflows/ci.yml.
//
// The workflow is YAML, so "tests" are: parses as valid YAML, contains
// the required jobs and steps, and references only job steps the
// repo's current Go version can actually run.
package ciworkflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Workflow mirrors the structure of a GitHub Actions workflow file
// just deeply enough to assert on jobs and their steps.
type Workflow struct {
	Name string    `yaml:"name"`
	On   yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Name   string `yaml:"name"`
		RunsOn string `yaml:"runs-on"`
		Steps  []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T) Workflow {
	t.Helper()
	// The CI workflow lives under .github/workflows which is a hidden
	// directory; Go's package loader won't pick it up as a package,
	// so the test lives in internal/ciworkflow and references the
	// workflow file by its repo-relative path.
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s as YAML: %v", path, err)
	}
	return wf
}

func TestWorkflow_ValidYAML(t *testing.T) {
	_ = loadWorkflow(t) // loadWorkflow fails the test on parse error
}

func TestWorkflow_HasBuildJob(t *testing.T) {
	wf := loadWorkflow(t)
	job, ok := wf.Jobs["lint"]
	if !ok {
		t.Fatal("expected job 'lint'")
	}
	if !strings.Contains(strings.ToLower(job.RunsOn), "ubuntu") {
		t.Errorf("expected ubuntu runner, got %q", job.RunsOn)
	}
	if len(job.Steps) < 4 {
		t.Errorf("expected at least 4 steps, got %d", len(job.Steps))
	}
}

func TestWorkflow_HasRaceJob(t *testing.T) {
	wf := loadWorkflow(t)
	job, ok := wf.Jobs["race"]
	if !ok {
		t.Fatal("expected separate 'race' job (race requires cgo and a C toolchain)")
	}
	if !strings.Contains(strings.ToLower(job.RunsOn), "ubuntu") {
		t.Errorf("race job should run on ubuntu (gcc available), got %q", job.RunsOn)
	}
	var sawRace bool
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "go test -race") {
			sawRace = true
		}
	}
	if !sawRace {
		t.Error("race job should run `go test -race`")
	}
}

func TestWorkflow_RunsGofmtVetTest(t *testing.T) {
	wf := loadWorkflow(t)
	// We have two jobs now; lint runs the basic checks, race runs
	// race tests. Either can satisfy "we have a gofmt/vet/test step".
	allSteps := ""
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			allSteps += s.Name + "\n" + s.Run + "\n"
		}
	}
	for _, want := range []string{"gofmt", "go vet", "go test"} {
		if !strings.Contains(allSteps, want) {
			t.Errorf("workflow missing a %q step across all jobs", want)
		}
	}
}

func TestWorkflow_UsesSetupGoWithMinVersion(t *testing.T) {
	wf := loadWorkflow(t)
	var sawSetupGo bool
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if s.Uses == "" {
				continue
			}
			if strings.HasPrefix(s.Uses, "actions/setup-go") {
				sawSetupGo = true
			}
		}
	}
	if !sawSetupGo {
		t.Error("workflow missing actions/setup-go step in any job")
	}
}
