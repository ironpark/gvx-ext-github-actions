package runners

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/github"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
)

// HostBackend is the subset of *extsdk.Host required by Engine A runner pools.
type HostBackend interface {
	SandboxList(ctx context.Context, ownedOnly bool) ([]extsdk.SandboxInfo, error)
	SandboxCreate(ctx context.Context, req extsdk.CreateSandboxRequest) (extsdk.SandboxInfo, error)
	SandboxStop(ctx context.Context, id string) (extsdk.SandboxInfo, error)
	SandboxDelete(ctx context.Context, id string) error
	SandboxLogsFollow(ctx context.Context, id string, tail int, onLine func(string) error) error
	SecretGet(ctx context.Context, key string) (string, bool, error)
}

// GitHubAPI is the subset of *github.Client used to mint single-use JIT configs.
type GitHubAPI interface {
	GenerateJITConfig(ctx context.Context, scope, target, token string, req github.JITConfigRequest) (github.JITConfigResponse, error)
	DeleteRunner(ctx context.Context, scope, target, token string, runnerID int64) error
}

var (
	reRunningJob   = regexp.MustCompile(`(?i)Running job:\s*(.+)$`)
	reJobCompleted = regexp.MustCompile(`(?i)Job\s+(.+?)\s+completed with result:\s*(\w+)`)
)

// Manager orchestrates Engine A (Official actions/runner Standby Ephemeral GVX Sandboxes).
// Each enabled RunnerConfig maintains up to Concurrency standby ephemeral sandboxes.
// After a sandbox finishes one job and exits, Manager immediately deletes the dirty
// sandbox, records the run in unified history, and provisions a clean replacement.
type Manager struct {
	store    *state.Store
	host     HostBackend
	gh       GitHubAPI
	redactor *mask.Redactor
	onChange func()

	pollInterval   time.Duration
	retryBackoff   time.Duration
	replenishDelay time.Duration

	mu      sync.Mutex
	loops   map[string]*runnerLoop
	closed  bool
	wg      sync.WaitGroup
}

type runnerLoop struct {
	cancel    context.CancelFunc
	slotsMu   sync.Mutex
	slots     []state.RunnerLiveSlot
	sandboxes map[string]struct{} // sandbox IDs currently managed by this loop
}

// NewManager creates an Engine A Standby Ephemeral Runner manager.
func NewManager(store *state.Store, host HostBackend, gh GitHubAPI, redactor *mask.Redactor, onChange func()) *Manager {
	if gh == nil {
		gh = github.NewClient()
	}
	if redactor == nil {
		redactor = mask.New()
	}
	return &Manager{
		store:          store,
		host:           host,
		gh:             gh,
		redactor:       redactor,
		onChange:       onChange,
		pollInterval:   2 * time.Second,
		retryBackoff:   5 * time.Second,
		replenishDelay: 500 * time.Millisecond,
		loops:          make(map[string]*runnerLoop),
	}
}

// Reconcile starts loops for all enabled runners and stops any disabled or deleted ones.
func (m *Manager) Reconcile() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	configs := m.store.Runners()
	want := make(map[string]state.RunnerConfig, len(configs))
	for _, cfg := range configs {
		if cfg.Enabled {
			want[cfg.ID] = cfg
		}
	}

	var toStop []string
	for id := range m.loops {
		if _, ok := want[id]; !ok {
			toStop = append(toStop, id)
		}
	}
	var toStart []state.RunnerConfig
	for id, cfg := range want {
		if _, running := m.loops[id]; !running {
			toStart = append(toStart, cfg)
		}
	}
	m.mu.Unlock()

	for _, id := range toStop {
		m.StopRunner(context.Background(), id)
	}
	for _, cfg := range toStart {
		m.startLoop(cfg)
	}
}

// StartRunner enables and starts a configured runner pool.
func (m *Manager) StartRunner(ctx context.Context, runnerID string) error {
	cfg, err := m.store.SetRunnerEnabled(ctx, runnerID, true)
	if err != nil {
		return err
	}
	m.startLoop(cfg)
	m.notify()
	return nil
}

// StopRunner disables a runner pool and cleans up its ephemeral standby sandboxes.
func (m *Manager) StopRunner(ctx context.Context, runnerID string) error {
	_, _ = m.store.SetRunnerEnabled(ctx, runnerID, false)

	m.mu.Lock()
	loop, ok := m.loops[runnerID]
	if ok {
		delete(m.loops, runnerID)
	}
	m.mu.Unlock()

	if ok && loop != nil {
		loop.cancel()
		m.cleanupLoopSandboxes(ctx, loop)
	}
	m.store.SetRunnerSlots(runnerID, nil)
	m.notify()
	return nil
}

// Shutdown cancels all runner loops and destroys all managed ephemeral sandboxes.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	loops := make([]*runnerLoop, 0, len(m.loops))
	for id, lp := range m.loops {
		loops = append(loops, lp)
		delete(m.loops, id)
	}
	m.mu.Unlock()

	for _, lp := range loops {
		lp.cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}

	cleanCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for _, lp := range loops {
		m.cleanupLoopSandboxes(cleanCtx, lp)
	}
}

func (m *Manager) cleanupLoopSandboxes(ctx context.Context, loop *runnerLoop) {
	if m.host == nil || loop == nil {
		return
	}
	loop.slotsMu.Lock()
	ids := make([]string, 0, len(loop.sandboxes))
	for id := range loop.sandboxes {
		ids = append(ids, id)
	}
	clear(loop.sandboxes)
	loop.slotsMu.Unlock()

	for _, sbxID := range ids {
		_ = m.host.SandboxDelete(ctx, sbxID)
	}
}

func (m *Manager) startLoop(cfg state.RunnerConfig) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, exists := m.loops[cfg.ID]; exists {
		m.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	conc := max(cfg.Concurrency, 1)
	if cfg.AuthMode == "jit" {
		conc = 1 // Manual JIT config is single-use.
	}
	slots := make([]state.RunnerLiveSlot, conc)
	for i := range conc {
		slots[i] = state.RunnerLiveSlot{
			SlotIndex: i + 1,
			Status:    "starting",
		}
	}
	lp := &runnerLoop{
		cancel:    cancel,
		slots:     slots,
		sandboxes: make(map[string]struct{}),
	}
	m.loops[cfg.ID] = lp
	m.store.SetRunnerSlots(cfg.ID, slots)
	for i := range conc {
		m.wg.Add(1)
		go m.runSlot(loopCtx, lp, cfg.ID, i)
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) updateSlot(runnerID string, lp *runnerLoop, idx int, fn func(*state.RunnerLiveSlot)) {
	lp.slotsMu.Lock()
	if idx >= 0 && idx < len(lp.slots) {
		fn(&lp.slots[idx])
	}
	snap := append([]state.RunnerLiveSlot(nil), lp.slots...)
	lp.slotsMu.Unlock()
	m.store.SetRunnerSlots(runnerID, snap)
	m.notify()
}

func (m *Manager) runSlot(ctx context.Context, lp *runnerLoop, runnerID string, idx int) {
	defer m.wg.Done()

	for {
		if ctx.Err() != nil {
			return
		}
		cfg, ok := m.store.RunnerByID(runnerID)
		if !ok || !cfg.Enabled {
			return
		}

		err := m.runOneEphemeralCycle(ctx, lp, cfg, idx)
		if ctx.Err() != nil {
			return
		}
		if cfg.AuthMode == "jit" {
			// Single-use manual JIT config completed (or failed); disable runner cleanly.
			_, _ = m.store.SetRunnerEnabled(context.Background(), cfg.ID, false)
			m.mu.Lock()
			delete(m.loops, cfg.ID)
			m.mu.Unlock()
			m.notify()
			return
		}

		wait := m.replenishDelay
		if err != nil {
			m.updateSlot(runnerID, lp, idx, func(sl *state.RunnerLiveSlot) {
				sl.Status = "error"
				sl.SandboxID = ""
				sl.RunID = ""
				sl.JobName = ""
				sl.LastError = m.redactor.SanitizeLine(err.Error())
			})
			wait = m.retryBackoff
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) runOneEphemeralCycle(ctx context.Context, lp *runnerLoop, cfg state.RunnerConfig, idx int) error {
	if m.host == nil {
		return errors.New("host API client unavailable")
	}

	m.updateSlot(cfg.ID, lp, idx, func(sl *state.RunnerLiveSlot) {
		sl.Status = "starting"
		sl.SandboxID = ""
		sl.RunID = ""
		sl.JobName = ""
		sl.LastError = ""
		sl.StartedAt = time.Now().UTC()
	})

	// 1. Obtain one-time JIT config (either via GitHub REST API using PAT from Host Vault,
	//    or directly from a manual JIT secret stored in Host Vault).
	secretKey := strings.TrimSpace(cfg.TokenSecretKey)
	if secretKey == "" {
		secretKey = "setting:github_pat"
	}
	secretVal, found, err := m.host.SecretGet(ctx, secretKey)
	if err != nil {
		return fmt.Errorf("read credential %q from vault: %w", secretKey, err)
	}
	if !found || strings.TrimSpace(secretVal) == "" {
		return fmt.Errorf("credential %q not set in Host Secrets Vault", secretKey)
	}
	m.redactor.Add("runner-cred:"+cfg.ID, secretVal)

	ephemeralName := fmt.Sprintf("%s-%d-%s", sanitizeSandboxName(cfg.Name), idx+1, state.NewID("s")[2:8])
	var (
		jitConfig      string
		remoteRunnerID int64
	)
	if cfg.AuthMode == "jit" {
		jitConfig = strings.TrimSpace(secretVal)
	} else {
		labels := cfg.Labels
		if len(labels) == 0 {
			labels = state.DefaultArchLabels(m.store.HostInfo().Arch)
		}
		jitResp, err := m.gh.GenerateJITConfig(ctx, cfg.Scope, cfg.Target, secretVal, github.JITConfigRequest{
			Name:          ephemeralName,
			RunnerGroupID: 1,
			Labels:        labels,
			WorkFolder:    "_work",
		})
		if err != nil {
			return err
		}
		jitConfig = jitResp.EncodedJITConfig
		remoteRunnerID = jitResp.Runner.ID
	}
	m.redactor.Add("runner-jit:"+ephemeralName, jitConfig)
	defer m.redactor.Remove("runner-jit:" + ephemeralName)

	// 2. Launch an ephemeral GVX sandbox running official actions/runner with --jitconfig.
	mem := strings.TrimSpace(cfg.Memory)
	if mem == "" {
		mem = "2g"
	}
	cpus := strings.TrimSpace(cfg.CPUs)
	if cpus == "" {
		cpus = "2"
	}
	img := strings.TrimSpace(cfg.Image)
	if img == "" {
		img = state.DefaultRunnerImage
	}

	startTrue := true
	cmd := `sh -c 'if [ -x ./run.sh ]; then exec ./run.sh --jitconfig "$GVX_RUNNER_JITCONFIG"; else exec /home/runner/run.sh --jitconfig "$GVX_RUNNER_JITCONFIG"; fi'`
	sbx, err := m.host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:            ephemeralName,
		Image:           img,
		Command:         cmd,
		Env:             []string{"GVX_RUNNER_JITCONFIG=" + jitConfig, "RUNNER_ALLOW_RUNASROOT=1"},
		Memory:          mem,
		CPUs:            cpus,
		Ephemeral:       true,
		MountDockerSock: cfg.AllowDocker,
		Start:           &startTrue,
	})
	if err != nil {
		if remoteRunnerID > 0 && cfg.AuthMode == "pat" {
			_ = m.gh.DeleteRunner(context.Background(), cfg.Scope, cfg.Target, secretVal, remoteRunnerID)
		}
		return fmt.Errorf("create ephemeral runner sandbox: %w", err)
	}

	lp.slotsMu.Lock()
	lp.sandboxes[sbx.ID] = struct{}{}
	lp.slotsMu.Unlock()

	// Guarantee dirty ephemeral sandbox destruction on exit.
	defer func() {
		m.updateSlot(cfg.ID, lp, idx, func(sl *state.RunnerLiveSlot) {
			sl.Status = "cleaning"
		})
		delCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		_ = m.host.SandboxDelete(delCtx, sbx.ID)
		cancel()

		lp.slotsMu.Lock()
		delete(lp.sandboxes, sbx.ID)
		lp.slotsMu.Unlock()
	}()

	m.updateSlot(cfg.ID, lp, idx, func(sl *state.RunnerLiveSlot) {
		sl.Status = "standby"
		sl.SandboxID = sbx.ID
		sl.Name = sbx.Name
	})

	// 3. Stream logs and watch sandbox status until it finishes its single job and exits.
	var (
		runMu       sync.Mutex
		activeRunID string
		jobResult   string
		jobName     string
		preJobLogs  []string
	)

	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()

	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		_ = m.host.SandboxLogsFollow(logCtx, sbx.ID, 200, func(raw string) error {
			clean := m.redactor.SanitizeLine(raw)
			if clean == "" {
				return nil
			}

			runMu.Lock()
			if strings.Contains(clean, "Listening for Jobs") && activeRunID == "" {
				runMu.Unlock()
				m.updateSlot(cfg.ID, lp, idx, func(sl *state.RunnerLiveSlot) {
					sl.Status = "standby"
				})
				return nil
			}
			if mJob := reRunningJob.FindStringSubmatch(clean); len(mJob) == 2 && activeRunID == "" {
				jobName = strings.TrimSpace(mJob[1])
				rec := m.store.AddRun(&state.RunRecord{
					Kind:       "runner",
					Status:     "running",
					StartedAt:  time.Now().UTC(),
					Target:     cfg.Target,
					RunnerID:   cfg.ID,
					RunnerName: cfg.Name,
					JobID:      jobName,
					SandboxID:  sbx.ID,
					Steps: []state.StepRecord{
						{
							ID:        "job:" + jobName,
							JobID:     jobName,
							Name:      jobName,
							Status:    "running",
							Detail:    "GitHub Self-Hosted Runner (" + sbx.Name + ")",
							StartedAt: time.Now().UTC(),
						},
					},
				})
				activeRunID = rec.ID
				for _, prev := range preJobLogs {
					m.store.AppendRunLog(activeRunID, prev)
				}
				preJobLogs = nil
				m.store.AppendRunLog(activeRunID, clean)
				runID := activeRunID
				jName := jobName
				runMu.Unlock()

				m.updateSlot(cfg.ID, lp, idx, func(sl *state.RunnerLiveSlot) {
					sl.Status = "running_job"
					sl.RunID = runID
					sl.JobName = jName
				})
				return nil
			}
			if mComp := reJobCompleted.FindStringSubmatch(clean); len(mComp) == 3 {
				jobResult = normalizeRunnerResult(mComp[2])
			}
			if activeRunID != "" {
				rid := activeRunID
				runMu.Unlock()
				m.store.AppendRunLog(rid, clean)
				m.notify()
				return nil
			}
			if len(preJobLogs) < 40 {
				preJobLogs = append(preJobLogs, clean)
			}
			runMu.Unlock()
			return nil
		})
	}()

	// Poll sandbox state until exited/stopped/removed (ephemeral runner exits once job completes).
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	waitLogs := func() {
		cancelLogs()
		if logDone != nil {
			<-logDone
			logDone = nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			waitLogs()
			runMu.Lock()
			rid := activeRunID
			runMu.Unlock()
			if rid != "" {
				m.finalizeRunnerRun(rid, "cancelled", "runner stopped")
			}
			return ctx.Err()

		case <-logDone:
			logDone = nil
			// Log stream closed cleanly; check final sandbox state.
			if !m.isSandboxActive(ctx, sbx.ID) {
				goto exited
			}

		case <-ticker.C:
			if !m.isSandboxActive(ctx, sbx.ID) {
				waitLogs()
				goto exited
			}
		}
	}

exited:
	runMu.Lock()
	rid := activeRunID
	res := jobResult
	runMu.Unlock()

	if rid != "" {
		if res == "" {
			res = "success"
		}
		errText := ""
		if res == "failure" {
			errText = "job failed on self-hosted runner"
		}
		m.finalizeRunnerRun(rid, res, errText)
	}
	return nil
}

func (m *Manager) isSandboxActive(ctx context.Context, sbxID string) bool {
	list, err := m.host.SandboxList(ctx, true)
	if err != nil {
		// Transient API error: assume still active until confirmed.
		return true
	}
	for _, sb := range list {
		if sb.ID == sbxID {
			st := strings.ToLower(strings.TrimSpace(sb.Status))
			return st == "running" || st == "starting" || st == "paused"
		}
	}
	return false
}

func (m *Manager) finalizeRunnerRun(runID, status, errMsg string) {
	now := time.Now().UTC()
	m.store.UpdateRun(runID, func(r *state.RunRecord) {
		r.Status = status
		r.FinishedAt = now
		r.Error = errMsg
		for i := range r.Steps {
			r.Steps[i].Status = status
			r.Steps[i].EndedAt = now
		}
	})
	pCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	_ = m.store.PersistHistory(pCtx)
	cancel()
	m.notify()
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

func normalizeRunnerResult(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "succeeded", "success":
		return "success"
	case "failed", "failure":
		return "failure"
	case "canceled", "cancelled":
		return "cancelled"
	default:
		return "success"
	}
}

func sanitizeSandboxName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "gvx-runner"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}
