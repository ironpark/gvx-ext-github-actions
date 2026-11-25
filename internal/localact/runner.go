package localact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	docker_container "github.com/moby/moby/api/types/container"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	"github.com/sirupsen/logrus"

	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
	"github.com/ironpark/gvx-ext-github-actions/internal/workflows"
)

// RunRequest describes one local workflow run triggered by UI, command, or MCP tool.
type RunRequest struct {
	WorkspacePath string
	WorkflowFile  string
	Event         string
	JobID         string // empty = all jobs for event
	PlatformImage string // mapped to ubuntu-latest / ubuntu-24.04 / ubuntu-22.04
	Architecture  string // e.g. "linux/arm64" or "linux/amd64"
	Inputs        map[string]string
	Secrets       map[string]string
	BindWorkdir   bool
	DryRun        bool
}

// Manager runs local GitHub Actions workflows via nektos/act against GVX_DOCKER_HOST.
type Manager struct {
	store    *state.Store
	redactor *mask.Redactor
	dataDir  string
	onChange func()

	mu            sync.Mutex
	upstreamHost  string
	shim          *DockerShim
	cancels       map[string]context.CancelFunc
	activeRunOrder []string // runIDs currently running, newest last
	runMu         sync.Mutex // serializes DOCKER_HOST env setup if needed
}

// NewManager creates a Local act workflow manager.
func NewManager(store *state.Store, redactor *mask.Redactor, dataDir, upstreamDockerHost string, onChange func()) *Manager {
	if redactor == nil {
		redactor = mask.New()
	}
	return &Manager{
		store:        store,
		redactor:     redactor,
		dataDir:      dataDir,
		upstreamHost: strings.TrimSpace(upstreamDockerHost),
		onChange:     onChange,
		cancels:      make(map[string]context.CancelFunc),
	}
}

// Start initializes the local Docker compatibility shim if GVX_DOCKER_HOST is configured.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shim != nil || m.upstreamHost == "" {
		return nil
	}
	shim, err := StartDockerShim(m.upstreamHost, m.handleContainerCreated)
	if err != nil {
		return err
	}
	m.shim = shim
	_ = os.Setenv("DOCKER_HOST", shim.DockerHost())
	return nil
}

// DockerHost returns the effective DOCKER_HOST endpoint used by act.
func (m *Manager) DockerHost() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shim != nil {
		return m.shim.DockerHost()
	}
	return m.upstreamHost
}

// Close cancels active local runs and stops the Docker compatibility shim.
func (m *Manager) Close() error {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for _, c := range m.cancels {
		cancels = append(cancels, c)
	}
	shim := m.shim
	m.shim = nil
	m.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	if shim != nil {
		return shim.Close()
	}
	return nil
}

func (m *Manager) handleContainerCreated(name, containerID string) {
	m.mu.Lock()
	var targetRunID string
	if len(m.activeRunOrder) > 0 {
		targetRunID = m.activeRunOrder[len(m.activeRunOrder)-1]
	}
	m.mu.Unlock()
	if targetRunID == "" {
		return
	}
	m.store.UpdateRun(targetRunID, func(r *state.RunRecord) {
		r.ContainerID = containerID
	})
	m.store.AppendRunLog(targetRunID, m.redactor.SanitizeLine(fmt.Sprintf("[gvx] container created: %s (%s)", name, shortID(containerID))))
	m.notify()
}

// StartRun validates the workflow, creates a RunRecord, and starts execution asynchronously.
func (m *Manager) StartRun(ctx context.Context, req RunRequest) (*state.RunRecord, error) {
	if strings.TrimSpace(req.WorkspacePath) == "" {
		return nil, errors.New("select a repository workspace first")
	}
	if strings.TrimSpace(req.WorkflowFile) == "" {
		return nil, errors.New("select a workflow file first")
	}
	if !req.DryRun {
		if m.DockerHost() == "" {
			return nil, errors.New("GVX_DOCKER_HOST is unavailable: enable Docker permission and start GVX Desktop's Sandbox Engine")
		}
		if err := m.Start(); err != nil {
			return nil, fmt.Errorf("start local Docker shim: %w", err)
		}
	}

	wfPath := filepath.Join(req.WorkspacePath, ".github", "workflows", filepath.Base(req.WorkflowFile))
	summary := workflows.ParseFile(wfPath)
	if summary.ParseError != "" {
		return nil, fmt.Errorf("workflow %s: %s", summary.File, summary.ParseError)
	}

	eventName := strings.TrimSpace(req.Event)
	if eventName == "" {
		if len(summary.Events) > 0 {
			eventName = summary.Events[0]
		} else {
			eventName = "push"
		}
	}

	img := strings.TrimSpace(req.PlatformImage)
	if img == "" {
		img = m.store.Settings().DefaultActImage
	}
	if img == "" {
		img = state.DefaultActImage
	}

	// Build initial DAG rows from the workflow jobs so the UI immediately renders the plan.
	initialSteps := buildInitialSteps(summary, req.JobID)

	rec := m.store.AddRun(&state.RunRecord{
		Kind:          "local",
		Status:        "running",
		StartedAt:     time.Now().UTC(),
		WorkspacePath: req.WorkspacePath,
		WorkflowFile:  summary.File,
		WorkflowName:  summary.Name,
		Event:         eventName,
		JobID:         req.JobID,
		PlatformImage: img,
		Steps:         initialSteps,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[rec.ID] = cancel
	m.activeRunOrder = append(m.activeRunOrder, rec.ID)
	m.mu.Unlock()

	m.notify()

	go m.execute(runCtx, rec.ID, wfPath, summary, req, eventName, img)
	return rec, nil
}

// CancelRun cancels a running local workflow by run ID.
func (m *Manager) CancelRun(runID string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	m.store.AppendRunLog(runID, "[gvx] cancellation requested by user")
	m.notify()
	return true
}

func (m *Manager) execute(ctx context.Context, runID, wfPath string, summary workflows.WorkflowSummary, req RunRequest, eventName, img string) {
	defer func() {
		m.mu.Lock()
		if cancel, ok := m.cancels[runID]; ok {
			cancel()
			delete(m.cancels, runID)
		}
		m.activeRunOrder = slices.DeleteFunc(m.activeRunOrder, func(id string) bool { return id == runID })
		m.mu.Unlock()

		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.store.PersistHistory(persistCtx)
		cancel()
		m.notify()
	}()

	// Register run secrets in redactor before emitting any logs.
	if len(req.Secrets) > 0 {
		for k, v := range req.Secrets {
			m.redactor.Add("run:"+runID+":"+k, v)
		}
		defer func() {
			for k := range req.Secrets {
				m.redactor.Remove("run:" + runID + ":" + k)
			}
		}()
	}

	m.appendLog(runID, fmt.Sprintf("[gvx] starting workflow %q (%s) on event %q with image %s", summary.Name, summary.File, eventName, img))

	planner, err := model.NewWorkflowPlanner(wfPath, true, false)
	if err != nil {
		m.finishRun(runID, "failure", fmt.Sprintf("plan workflow: %v", err))
		return
	}

	var plan *model.Plan
	if strings.TrimSpace(req.JobID) != "" {
		plan, err = planner.PlanJob(strings.TrimSpace(req.JobID))
	} else {
		plan, err = planner.PlanEvent(eventName)
	}
	if err != nil {
		m.finishRun(runID, "failure", fmt.Sprintf("build execution plan: %v", err))
		return
	}
	if plan == nil || len(plan.Stages) == 0 {
		m.finishRun(runID, "failure", fmt.Sprintf("no jobs matched event %q (job=%q)", eventName, req.JobID))
		return
	}

	platforms := map[string]string{
		"ubuntu-latest": img,
		"ubuntu-24.04":  img,
		"ubuntu-22.04":  img,
		"ubuntu-20.04":  img,
		"self-hosted":   img,
	}
	for _, j := range summary.Jobs {
		for _, label := range j.RunsOn {
			key := strings.ToLower(strings.TrimSpace(label))
			if key != "" {
				if _, exists := platforms[key]; !exists {
					platforms[key] = img
				}
			}
		}
	}

	cacheDir := filepath.Join(os.TempDir(), "gvx-act-cache")
	if strings.TrimSpace(m.dataDir) != "" {
		cacheDir = filepath.Join(m.dataDir, "act-cache")
	}
	_ = os.MkdirAll(cacheDir, 0o700)

	// Determine container network mode: bridge by default (host networking is
	// intentionally blocked by GVX's scoped Docker proxy).
	netMode := docker_container.NetworkMode("bridge")

	cfg := &runner.Config{
		Actor:                 "gvx-desktop",
		Workdir:               req.WorkspacePath,
		ActionCacheDir:        cacheDir,
		BindWorkdir:           req.BindWorkdir,
		ReuseContainers:       req.DryRun, // Prevents act from dialing Docker to remove volumes during DryRun.
		EventName:             eventName,
		DefaultBranch:         "main",
		LogOutput:             true,
		Inputs:                req.Inputs,
		Secrets:               req.Secrets,
		Token:                 req.Secrets["GITHUB_TOKEN"],
		Platforms:             platforms,
		ContainerArchitecture: strings.TrimSpace(req.Architecture),
		ContainerDaemonSocket: "-", // Secure default: do not mount host Docker socket into job containers.
		ContainerNetworkMode:  netMode,
		UseGitIgnore:          true,
		GitHubInstance:        "github.com",
		AutoRemove:            true,
		ConcurrentJobs:        1,
	}

	r, err := runner.New(cfg)
	if err != nil {
		m.finishRun(runID, "failure", fmt.Sprintf("configure act runner: %v", err))
		return
	}

	hook := &actLogHook{
		mgr:   m,
		runID: runID,
	}
	factory := &actLoggerFactory{hook: hook}

	execCtx := ctx
	if req.DryRun {
		execCtx = common.WithDryrun(execCtx, true)
	}
	execCtx = common.WithLogger(execCtx, factory.WithJobLogger())
	execCtx = runner.WithJobLoggerFactory(execCtx, factory)

	execErr := r.NewPlanExecutor(plan)(execCtx)
	switch {
	case execErr == nil:
		m.markPendingStepsSkipped(runID)
		m.finishRun(runID, "success", "")
	case errors.Is(ctx.Err(), context.Canceled):
		m.finishRun(runID, "cancelled", "cancelled by user")
	default:
		m.finishRun(runID, "failure", m.redactor.SanitizeLine(execErr.Error()))
	}
}

func (m *Manager) finishRun(runID, status, errMsg string) {
	now := time.Now().UTC()
	m.store.UpdateRun(runID, func(r *state.RunRecord) {
		r.Status = status
		r.FinishedAt = now
		r.Error = errMsg
	})
	if errMsg != "" {
		m.appendLog(runID, fmt.Sprintf("[gvx] finished (%s): %s", status, errMsg))
	} else {
		m.appendLog(runID, fmt.Sprintf("[gvx] finished (%s)", status))
	}
	m.notify()
}

func (m *Manager) markPendingStepsSkipped(runID string) {
	m.store.UpdateRun(runID, func(r *state.RunRecord) {
		for i := range r.Steps {
			if r.Steps[i].Status == "running" {
				r.Steps[i].Status = "success"
				r.Steps[i].EndedAt = time.Now().UTC()
			}
		}
	})
}

func (m *Manager) appendLog(runID, rawLine string) {
	clean := m.redactor.SanitizeLine(rawLine)
	if clean == "" {
		return
	}
	m.store.AppendRunLog(runID, clean)
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

type actLoggerFactory struct {
	hook logrus.Hook
}

func (f *actLoggerFactory) WithJobLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.InfoLevel)
	l.AddHook(f.hook)
	return l
}

type actLogHook struct {
	mgr        *Manager
	runID      string
	mu         sync.Mutex
	lastNotify time.Time
}

func (h *actLogHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
	}
}

func (h *actLogHook) Fire(entry *logrus.Entry) error {
	jobID, _ := entry.Data["jobID"].(string)
	stepName, _ := entry.Data["step"].(string)
	stepResult, _ := entry.Data["stepResult"].(string)
	jobResult, _ := entry.Data["jobResult"].(string)
	rawOut, _ := entry.Data["raw_output"].(bool)

	var stepID string
	switch v := entry.Data["stepID"].(type) {
	case []string:
		if len(v) > 0 {
			stepID = strings.Join(v, "/")
		}
	case string:
		stepID = v
	}
	if stepID == "" {
		if v, ok := entry.Data["stepid"].([]string); ok && len(v) > 0 {
			stepID = strings.Join(v, "/")
		}
	}

	msg := strings.TrimSpace(entry.Message)
	if msg != "" {
		var line string
		switch {
		case rawOut:
			if jobID != "" {
				line = fmt.Sprintf("[%s] | %s", jobID, msg)
			} else {
				line = "| " + msg
			}
		case jobID != "" && stepName != "":
			line = fmt.Sprintf("[%s / %s] %s", jobID, stepName, msg)
		case jobID != "":
			line = fmt.Sprintf("[%s] %s", jobID, msg)
		default:
			line = msg
		}
		h.mgr.appendLog(h.runID, line)
	}

	// Update job and step DAG status nodes.
	if jobID != "" || stepName != "" || stepResult != "" || jobResult != "" {
		now := time.Now().UTC()
		h.mgr.store.UpdateRun(h.runID, func(r *state.RunRecord) {
			if jobID != "" {
				jNodeID := "job:" + jobID
				jIdx := findStepIdx(r.Steps, jNodeID)
				if jIdx < 0 {
					r.Steps = append(r.Steps, state.StepRecord{
						ID:        jNodeID,
						JobID:     jobID,
						Name:      jobID,
						Status:    "running",
						StartedAt: now,
					})
					jIdx = len(r.Steps) - 1
				}
				if jobResult != "" {
					r.Steps[jIdx].Status = normalizeResultStatus(jobResult)
					r.Steps[jIdx].EndedAt = now
				} else if r.Steps[jIdx].Status == "pending" {
					r.Steps[jIdx].Status = "running"
					r.Steps[jIdx].StartedAt = now
				}
			}

			if stepName != "" {
				sKey := stepID
				if sKey == "" {
					sKey = slugID(stepName)
				}
				sNodeID := fmt.Sprintf("step:%s:%s", jobID, sKey)
				if len(sNodeID) > 60 {
					sNodeID = sNodeID[:60]
				}
				sIdx := findStepIdx(r.Steps, sNodeID)
				if sIdx < 0 {
					var parents []string
					if jobID != "" {
						parents = []string{"job:" + jobID}
					}
					r.Steps = append(r.Steps, state.StepRecord{
						ID:        sNodeID,
						JobID:     jobID,
						Name:      mask.TruncateRunes(mask.SanitizeInline(stepName), 120),
						Parents:   parents,
						Status:    "running",
						StartedAt: now,
					})
					sIdx = len(r.Steps) - 1
				}
				if stepResult != "" {
					r.Steps[sIdx].Status = normalizeResultStatus(stepResult)
					r.Steps[sIdx].EndedAt = now
				} else if r.Steps[sIdx].Status == "pending" {
					r.Steps[sIdx].Status = "running"
					r.Steps[sIdx].StartedAt = now
				}
			}
		})
	}

	h.mu.Lock()
	shouldNotify := stepResult != "" || jobResult != "" || time.Since(h.lastNotify) >= 120*time.Millisecond
	if shouldNotify {
		h.lastNotify = time.Now()
	}
	h.mu.Unlock()
	if shouldNotify {
		h.mgr.notify()
	}
	return nil
}

func buildInitialSteps(wf workflows.WorkflowSummary, onlyJob string) []state.StepRecord {
	var out []state.StepRecord
	for _, j := range wf.Jobs {
		if onlyJob != "" && j.ID != onlyJob {
			continue
		}
		var parents []string
		for _, need := range j.Needs {
			parents = append(parents, "job:"+need)
		}
		out = append(out, state.StepRecord{
			ID:      "job:" + j.ID,
			JobID:   j.ID,
			Name:    j.Name,
			Parents: parents,
			Status:  "pending",
			Detail:  strings.Join(j.RunsOn, ", "),
		})
	}
	return out
}

func findStepIdx(steps []state.StepRecord, id string) int {
	for i := range steps {
		if steps[i].ID == id {
			return i
		}
	}
	return -1
}

func normalizeResultStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "success", "succeeded":
		return "success"
	case "failure", "failed":
		return "failure"
	case "skipped":
		return "skipped"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "running"
	}
}

func slugID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
		if b.Len() >= 28 {
			break
		}
	}
	if b.Len() == 0 {
		return "step"
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
