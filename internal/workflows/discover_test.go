package workflows

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverWorkflows(t *testing.T) {
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ci := `name: CI
on:
  push:
  workflow_dispatch:
    inputs:
      env:
        description: Target environment
        default: staging
        type: choice
        options:
          - staging
          - prod
jobs:
  lint:
    name: Lint
    runs-on: ubuntu-latest
    steps:
      - name: Run linter
        run: echo lint
  test:
    name: Unit Tests
    needs: lint
    runs-on: ubuntu-latest
    steps:
      - name: Go test
        run: go test ./...
`
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(ci), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "broken.yaml"), []byte(":::not valid yaml::: ["), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(list))
	}

	// broken.yaml comes first alphabetically.
	if list[0].File != "broken.yaml" || list[0].ParseError == "" {
		t.Fatalf("expected broken.yaml with ParseError, got %+v", list[0])
	}

	got := list[1]
	if got.File != "ci.yml" || got.Name != "CI" || got.ParseError != "" {
		t.Fatalf("unexpected ci.yml summary: %+v", got)
	}
	if len(got.Events) != 2 || got.Events[0] != "push" || got.Events[1] != "workflow_dispatch" {
		t.Fatalf("events = %v", got.Events)
	}
	if len(got.DispatchInputs) != 1 || got.DispatchInputs[0].Name != "env" || got.DispatchInputs[0].Default != "staging" {
		t.Fatalf("dispatch inputs = %+v", got.DispatchInputs)
	}
	if len(got.Jobs) != 2 {
		t.Fatalf("jobs = %+v", got.Jobs)
	}
	if got.Jobs[1].ID != "test" || len(got.Jobs[1].Needs) != 1 || got.Jobs[1].Needs[0] != "lint" {
		t.Fatalf("test job = %+v", got.Jobs[1])
	}
	if !got.HasAMD64OnlyHint("arm64") {
		t.Fatal("expected arm64 hint for ubuntu-latest")
	}
	if got.HasAMD64OnlyHint("amd64") {
		t.Fatal("unexpected arm64 hint on amd64")
	}
}
