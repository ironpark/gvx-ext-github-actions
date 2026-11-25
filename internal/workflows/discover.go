package workflows

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nektos/act/pkg/model"
)

// DispatchInput describes one workflow_dispatch input parameter.
type DispatchInput struct {
	Name        string
	Description string
	Required    bool
	Default     string
	Type        string
	Options     []string
}

// JobSummary summarizes one job inside a workflow.
type JobSummary struct {
	ID         string
	Name       string
	Needs      []string
	RunsOn     []string
	Steps      []string
	HasService bool
}

// WorkflowSummary summarizes one parsed .github/workflows/*.{yml,yaml} file.
type WorkflowSummary struct {
	File           string // basename, e.g. "ci.yml"
	Path           string // full path on disk
	Name           string
	Events         []string
	Jobs           []JobSummary
	DispatchInputs []DispatchInput
	ParseError     string
}

// Discover scans <repoPath>/.github/workflows for .yml/.yaml files and parses them
// using nektos/act's workflow model. Invalid YAML files are returned with ParseError
// populated instead of failing the entire directory scan.
func Discover(repoPath string) ([]WorkflowSummary, error) {
	wfDir := filepath.Join(repoPath, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", wfDir, err)
	}

	var out []WorkflowSummary
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		fullPath := filepath.Join(wfDir, entry.Name())
		out = append(out, ParseFile(fullPath))
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].File < out[j].File
	})
	return out, nil
}

// ParseFile parses a single GitHub Actions workflow YAML file.
func ParseFile(fullPath string) WorkflowSummary {
	base := filepath.Base(fullPath)
	summary := WorkflowSummary{
		File: base,
		Path: fullPath,
		Name: base,
	}

	f, err := os.Open(fullPath)
	if err != nil {
		summary.ParseError = err.Error()
		return summary
	}
	defer f.Close()

	wf, err := model.ReadWorkflow(f, false)
	if err != nil {
		summary.ParseError = err.Error()
		return summary
	}
	if strings.TrimSpace(wf.Name) != "" {
		summary.Name = strings.TrimSpace(wf.Name)
	}

	events := wf.On()
	sort.Strings(events)
	events = slices.Compact(events)
	if len(events) == 0 {
		events = []string{"push"}
	}
	summary.Events = events

	if disp := wf.WorkflowDispatchConfig(); disp != nil && len(disp.Inputs) > 0 {
		names := make([]string, 0, len(disp.Inputs))
		for k := range disp.Inputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			in := disp.Inputs[k]
			summary.DispatchInputs = append(summary.DispatchInputs, DispatchInput{
				Name:        k,
				Description: in.Description,
				Required:    in.Required,
				Default:     in.Default,
				Type:        in.Type,
				Options:     slices.Clone(in.Options),
			})
		}
	}

	jobIDs := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		jobIDs = append(jobIDs, id)
	}
	sort.Strings(jobIDs)

	for _, id := range jobIDs {
		j := wf.Jobs[id]
		if j == nil {
			continue
		}
		jName := strings.TrimSpace(j.Name)
		if jName == "" {
			jName = id
		}
		steps := make([]string, 0, len(j.Steps))
		for idx, st := range j.Steps {
			if st == nil {
				continue
			}
			label := strings.TrimSpace(st.Name)
			if label == "" {
				label = strings.TrimSpace(st.String())
			}
			if label == "" {
				label = fmt.Sprintf("Step %d", idx+1)
			}
			steps = append(steps, label)
		}
		summary.Jobs = append(summary.Jobs, JobSummary{
			ID:         id,
			Name:       jName,
			Needs:      slices.Clone(j.Needs()),
			RunsOn:     slices.Clone(j.RunsOn()),
			Steps:      steps,
			HasService: len(j.Services) > 0,
		})
	}
	return summary
}

// HasAMD64OnlyHint reports whether any runs-on label in the workflow targets standard
// GitHub-hosted ubuntu/windows/macos labels where amd64-only binaries may differ on arm64.
func (w WorkflowSummary) HasAMD64OnlyHint(hostArch string) bool {
	if !strings.EqualFold(hostArch, "arm64") && !strings.EqualFold(hostArch, "aarch64") {
		return false
	}
	for _, j := range w.Jobs {
		for _, label := range j.RunsOn {
			l := strings.ToLower(strings.TrimSpace(label))
			if strings.HasPrefix(l, "ubuntu-") || l == "ubuntu-latest" {
				return true
			}
		}
	}
	return false
}
