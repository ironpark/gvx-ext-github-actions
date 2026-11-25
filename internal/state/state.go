package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

const (
	StorageKeyWorkspaces = "workspaces_v1"
	StorageKeyRunners    = "runners_v1"
	StorageKeyHistory    = "history_v1"

	DefaultActImage    = "catthehacker/ubuntu:act-latest"
	DefaultRunnerImage = "ghcr.io/actions/actions-runner:latest"
	MaxHistoryEntries  = 80
	MaxLogsPerRun      = 500
	MaxLogBytesPerRun  = 24 << 10 // Keep total history storage well under 4 MiB KV quota
)

// KVStore is the minimal Host storage interface used by Store.
type KVStore interface {
	StorageGetJSON(ctx context.Context, key string, dst any) (bool, error)
	StorageSetJSON(ctx context.Context, key string, v any) error
}

// Workspace represents an approved local Git repository registered in Tab 1.
type Workspace struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	AddedAt time.Time `json:"added_at"`
}

// RunnerConfig configures an Engine A (Official actions/runner Standby Ephemeral Sandbox) runner pool.
type RunnerConfig struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Scope          string    `json:"scope"`            // "repo" or "org"
	Target         string    `json:"target"`           // "owner/repo" or "org"
	AuthMode       string    `json:"auth_mode"`        // "pat" (GitHub PAT -> JIT API) or "jit" (manual encoded JIT config)
	TokenSecretKey string    `json:"token_secret_key"` // Host Vault key (or "setting:github_pat")
	Labels         []string  `json:"labels"`
	Image          string    `json:"image"`
	Concurrency    int       `json:"concurrency"`
	AllowDocker    bool      `json:"allow_docker"`
	Memory         string    `json:"memory,omitempty"`
	CPUs           string    `json:"cpus,omitempty"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
}

// RunnerLiveSlot tracks a running ephemeral standby or job-executing sandbox.
type RunnerLiveSlot struct {
	SlotIndex int       `json:"slot_index"`
	SandboxID string    `json:"sandbox_id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // "starting", "standby", "running_job", "cleaning", "error"
	RunID     string    `json:"run_id,omitempty"`
	JobName   string    `json:"job_name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	LastError string    `json:"last_error,omitempty"`
}

// StepRecord tracks one Job or Step node in a local/runner execution DAG.
type StepRecord struct {
	ID        string   `json:"id"`
	JobID     string   `json:"job_id,omitempty"`
	Name      string   `json:"name"`
	Parents   []string `json:"parents,omitempty"`
	Status    string   `json:"status"` // "pending", "running", "success", "failure", "skipped"
	Detail    string   `json:"detail,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// RunRecord is a unified run history record shared by Tab 1 (local act), Tab 2 (self-hosted), and Tab 3 (History).
type RunRecord struct {
	ID            string       `json:"id"`
	Kind          string       `json:"kind"`   // "local" or "runner"
	Status        string       `json:"status"` // "queued", "running", "success", "failure", "cancelled"
	StartedAt     time.Time    `json:"started_at"`
	FinishedAt    time.Time    `json:"finished_at,omitempty"`
	WorkspacePath string       `json:"workspace_path,omitempty"`
	WorkflowFile  string       `json:"workflow_file,omitempty"`
	WorkflowName  string       `json:"workflow_name,omitempty"`
	Event         string       `json:"event,omitempty"`
	JobID         string       `json:"job_id,omitempty"`
	PlatformImage string       `json:"platform_image,omitempty"`
	Target        string       `json:"target,omitempty"` // owner/repo or org
	RunnerID      string       `json:"runner_id,omitempty"`
	RunnerName    string       `json:"runner_name,omitempty"`
	ContainerID   string       `json:"container_id,omitempty"`
	SandboxID     string       `json:"sandbox_id,omitempty"`
	Error         string       `json:"error,omitempty"`
	Steps         []StepRecord `json:"steps,omitempty"`
	Logs          []string     `json:"logs,omitempty"`
	logBytes      int
}

// Settings caches non-secret user settings from Host /v1/settings.
type Settings struct {
	DefaultActImage    string
	DefaultConcurrency int
	HasDefaultPAT      bool
}

// Store holds extension state in memory and persists durable slices to Host KV storage.
type Store struct {
	mu         sync.RWMutex
	kv         KVStore
	hostInfo   extsdk.HostInfo
	settings   Settings
	workspaces []Workspace
	runners    []RunnerConfig
	liveSlots  map[string][]RunnerLiveSlot // runnerID -> active slots
	history    []*RunRecord                // newest first
	secretKeys []string
}

// NewStore initializes a Store with sensible defaults.
func NewStore(kv KVStore) *Store {
	return &Store{
		kv: kv,
		settings: Settings{
			DefaultActImage:    DefaultActImage,
			DefaultConcurrency: 1,
		},
		liveSlots: make(map[string][]RunnerLiveSlot),
	}
}

// Load restores workspaces, runners, and run history from Host KV storage.
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	var workspaces []Workspace
	if _, err := s.kv.StorageGetJSON(ctx, StorageKeyWorkspaces, &workspaces); err != nil {
		return fmt.Errorf("load workspaces: %w", err)
	}
	var runners []RunnerConfig
	if _, err := s.kv.StorageGetJSON(ctx, StorageKeyRunners, &runners); err != nil {
		return fmt.Errorf("load runners: %w", err)
	}
	var history []*RunRecord
	if _, err := s.kv.StorageGetJSON(ctx, StorageKeyHistory, &history); err != nil {
		return fmt.Errorf("load history: %w", err)
	}

	// Any run left in "running" or "queued" from a previous crash becomes "cancelled".
	for _, r := range history {
		if r == nil {
			continue
		}
		if r.Status == "running" || r.Status == "queued" {
			r.Status = "cancelled"
			if r.FinishedAt.IsZero() {
				r.FinishedAt = time.Now().UTC()
			}
			if r.Error == "" {
				r.Error = "interrupted by extension restart"
			}
		}
		for _, ln := range r.Logs {
			r.logBytes += len(ln) + 1
		}
	}

	s.mu.Lock()
	s.workspaces = workspaces
	s.runners = runners
	s.history = history
	s.mu.Unlock()
	return nil
}

func (s *Store) SetHostInfo(info extsdk.HostInfo) {
	s.mu.Lock()
	s.hostInfo = info
	s.mu.Unlock()
}

func (s *Store) HostInfo() extsdk.HostInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hostInfo
}

func (s *Store) ApplySettings(raw map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.DefaultActImage = DefaultActImage
	s.settings.DefaultConcurrency = 1
	s.settings.HasDefaultPAT = false
	if raw == nil {
		return
	}
	if v, ok := raw["default_act_image"].(string); ok && strings.TrimSpace(v) != "" {
		s.settings.DefaultActImage = strings.TrimSpace(v)
	}
	switch v := raw["default_concurrency"].(type) {
	case float64:
		if int(v) >= 1 && int(v) <= 8 {
			s.settings.DefaultConcurrency = int(v)
		}
	case int:
		if v >= 1 && v <= 8 {
			s.settings.DefaultConcurrency = v
		}
	}
	if m, ok := raw["github_pat"].(map[string]any); ok {
		if isSet, ok := m["set"].(bool); ok {
			s.settings.HasDefaultPAT = isSet
		}
	}
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Store) SetSecretKeys(keys []string) {
	cp := slices.Clone(keys)
	slices.Sort(cp)
	s.mu.Lock()
	s.secretKeys = cp
	s.mu.Unlock()
}

func (s *Store) SecretKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.secretKeys)
}

// --- Workspaces ---

func (s *Store) Workspaces() []Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.workspaces)
}

func (s *Store) AddWorkspace(ctx context.Context, path string) (Workspace, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return Workspace{}, errors.New("workspace path is empty")
	}
	s.mu.Lock()
	for _, w := range s.workspaces {
		if w.Path == clean {
			s.mu.Unlock()
			return w, nil
		}
	}
	ws := Workspace{
		Path:    clean,
		Name:    filepath.Base(clean),
		AddedAt: time.Now().UTC(),
	}
	s.workspaces = append(s.workspaces, ws)
	snap := slices.Clone(s.workspaces)
	s.mu.Unlock()

	if s.kv != nil {
		if err := s.kv.StorageSetJSON(ctx, StorageKeyWorkspaces, snap); err != nil {
			return ws, err
		}
	}
	return ws, nil
}

func (s *Store) RemoveWorkspace(ctx context.Context, path string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	s.mu.Lock()
	s.workspaces = slices.DeleteFunc(s.workspaces, func(w Workspace) bool {
		return w.Path == clean
	})
	snap := slices.Clone(s.workspaces)
	s.mu.Unlock()

	if s.kv != nil {
		return s.kv.StorageSetJSON(ctx, StorageKeyWorkspaces, snap)
	}
	return nil
}

// --- Runners ---

func (s *Store) Runners() []RunnerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RunnerConfig, len(s.runners))
	for i, r := range s.runners {
		r.Labels = slices.Clone(r.Labels)
		out[i] = r
	}
	return out
}

func (s *Store) RunnerByID(id string) (RunnerConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runners {
		if r.ID == id {
			r.Labels = slices.Clone(r.Labels)
			return r, true
		}
	}
	return RunnerConfig{}, false
}

func (s *Store) SaveRunner(ctx context.Context, cfg RunnerConfig) (RunnerConfig, error) {
	if strings.TrimSpace(cfg.Target) == "" {
		return RunnerConfig{}, errors.New("runner target is required")
	}
	if cfg.ID == "" {
		cfg.ID = NewID("runr")
	}
	if cfg.Scope != "org" {
		cfg.Scope = "repo"
	}
	if cfg.AuthMode != "jit" {
		cfg.AuthMode = "pat"
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = sanitizeSlug("gvx-" + cfg.Target)
	}
	if strings.TrimSpace(cfg.Image) == "" {
		cfg.Image = DefaultRunnerImage
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Concurrency > 8 {
		cfg.Concurrency = 8
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now().UTC()
	}
	cfg.Labels = slices.Clone(cfg.Labels)

	s.mu.Lock()
	replaced := false
	for i := range s.runners {
		if s.runners[i].ID == cfg.ID {
			s.runners[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		s.runners = append(s.runners, cfg)
	}
	snap := make([]RunnerConfig, len(s.runners))
	copy(snap, s.runners)
	s.mu.Unlock()

	if s.kv != nil {
		if err := s.kv.StorageSetJSON(ctx, StorageKeyRunners, snap); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func (s *Store) SetRunnerEnabled(ctx context.Context, id string, enabled bool) (RunnerConfig, error) {
	s.mu.Lock()
	var found RunnerConfig
	ok := false
	for i := range s.runners {
		if s.runners[i].ID == id {
			s.runners[i].Enabled = enabled
			found = s.runners[i]
			found.Labels = slices.Clone(found.Labels)
			ok = true
			break
		}
	}
	snap := make([]RunnerConfig, len(s.runners))
	copy(snap, s.runners)
	s.mu.Unlock()
	if !ok {
		return RunnerConfig{}, fmt.Errorf("runner %q not found", id)
	}
	if s.kv != nil {
		if err := s.kv.StorageSetJSON(ctx, StorageKeyRunners, snap); err != nil {
			return found, err
		}
	}
	return found, nil
}

func (s *Store) DeleteRunner(ctx context.Context, id string) error {
	s.mu.Lock()
	s.runners = slices.DeleteFunc(s.runners, func(r RunnerConfig) bool {
		return r.ID == id
	})
	delete(s.liveSlots, id)
	snap := make([]RunnerConfig, len(s.runners))
	copy(snap, s.runners)
	s.mu.Unlock()

	if s.kv != nil {
		return s.kv.StorageSetJSON(ctx, StorageKeyRunners, snap)
	}
	return nil
}

func (s *Store) SetRunnerSlots(runnerID string, slots []RunnerLiveSlot) {
	cp := slices.Clone(slots)
	s.mu.Lock()
	if len(cp) == 0 {
		delete(s.liveSlots, runnerID)
	} else {
		s.liveSlots[runnerID] = cp
	}
	s.mu.Unlock()
}

func (s *Store) RunnerSlots(runnerID string) []RunnerLiveSlot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.liveSlots[runnerID])
}

func (s *Store) AllRunnerSlots() map[string][]RunnerLiveSlot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]RunnerLiveSlot, len(s.liveSlots))
	for k, v := range s.liveSlots {
		out[k] = slices.Clone(v)
	}
	return out
}

// --- Unified Run History ---

func (s *Store) AddRun(rec *RunRecord) *RunRecord {
	if rec.ID == "" {
		rec.ID = NewID("run")
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().UTC()
	}
	if rec.Status == "" {
		rec.Status = "running"
	}
	s.mu.Lock()
	s.history = append([]*RunRecord{cloneRunRecord(rec)}, s.history...)
	if len(s.history) > MaxHistoryEntries {
		s.history = s.history[:MaxHistoryEntries]
	}
	out := cloneRunRecord(s.history[0])
	s.mu.Unlock()
	return out
}

func (s *Store) UpdateRun(id string, fn func(r *RunRecord)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.history {
		if r.ID == id {
			fn(r)
			return true
		}
	}
	return false
}

// AppendRunLog appends a sanitized, secret-masked log line to run id, keeping
// memory and persisted KV storage bounded.
func (s *Store) AppendRunLog(id, line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.history {
		if r.ID != id {
			continue
		}
		r.Logs = append(r.Logs, line)
		r.logBytes += len(line) + 1
		for len(r.Logs) > MaxLogsPerRun || (len(r.Logs) > 1 && r.logBytes > MaxLogBytesPerRun) {
			r.logBytes -= len(r.Logs[0]) + 1
			r.Logs = r.Logs[1:]
		}
		return true
	}
	return false
}

func (s *Store) PersistHistory(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	snap := s.History()
	return s.kv.StorageSetJSON(ctx, StorageKeyHistory, snap)
}

func (s *Store) History() []*RunRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RunRecord, len(s.history))
	for i, r := range s.history {
		out[i] = cloneRunRecord(r)
	}
	return out
}

func (s *Store) RunByID(id string) (*RunRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.history {
		if r.ID == id {
			return cloneRunRecord(r), true
		}
	}
	return nil, false
}

func (s *Store) ClearFinishedHistory(ctx context.Context) error {
	s.mu.Lock()
	kept := make([]*RunRecord, 0, len(s.history))
	for _, r := range s.history {
		if r.Status == "running" || r.Status == "queued" {
			kept = append(kept, r)
		}
	}
	s.history = kept
	s.mu.Unlock()
	return s.PersistHistory(ctx)
}

// ActiveCounts returns (runningLocalRuns, activeRunnerSlots, runningRunnerJobs).
func (s *Store) ActiveCounts() (localRunning, runnerSlots, runnerJobs int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.history {
		if r.Kind == "local" && (r.Status == "running" || r.Status == "queued") {
			localRunning++
		}
	}
	for _, slots := range s.liveSlots {
		for _, sl := range slots {
			if sl.Status == "standby" || sl.Status == "starting" || sl.Status == "running_job" {
				runnerSlots++
			}
			if sl.Status == "running_job" {
				runnerJobs++
			}
		}
	}
	return localRunning, runnerSlots, runnerJobs
}

func cloneRunRecord(r *RunRecord) *RunRecord {
	if r == nil {
		return nil
	}
	cp := *r
	if len(r.Steps) > 0 {
		cp.Steps = make([]StepRecord, len(r.Steps))
		for i, st := range r.Steps {
			st.Parents = slices.Clone(st.Parents)
			cp.Steps[i] = st
		}
	}
	cp.Logs = slices.Clone(r.Logs)
	return &cp
}

// NewID generates a short random identifier prefixed with prefix.
func NewID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

func sanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '/' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "gvx-runner"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// DefaultArchLabels returns recommended self-hosted runner labels for the host arch.
func DefaultArchLabels(arch string) []string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "arm64", "aarch64":
		return []string{"self-hosted", "linux", "arm64", "gvx"}
	case "amd64", "x86_64":
		return []string{"self-hosted", "linux", "x64", "gvx"}
	default:
		return []string{"self-hosted", "linux", "gvx"}
	}
}
