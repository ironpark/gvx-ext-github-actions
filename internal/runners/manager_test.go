package runners

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/github"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
)

type fakeGH struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeGH) GenerateJITConfig(_ context.Context, scope, target, token string, req github.JITConfigRequest) (github.JITConfigResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return github.JITConfigResponse{
		Runner:           github.RemoteRunner{ID: int64(100 + n), Name: req.Name},
		EncodedJITConfig: fmt.Sprintf("jit-secret-token-%d", n),
	}, nil
}

func (f *fakeGH) DeleteRunner(_ context.Context, _, _, _ string, _ int64) error { return nil }

type fakeHost struct {
	mu          sync.Mutex
	secrets     map[string]string
	sandboxes   map[string]extsdk.SandboxInfo
	createReqs  []extsdk.CreateSandboxRequest
	deletedIDs  []string
	createdSeq  int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		secrets:   map[string]string{"setting:github_pat": "ghp_super_secret_pat_123"},
		sandboxes: map[string]extsdk.SandboxInfo{},
	}
}

func (h *fakeHost) SecretGet(_ context.Context, key string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.secrets[key]
	return v, ok, nil
}

func (h *fakeHost) SandboxList(_ context.Context, _ bool) ([]extsdk.SandboxInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]extsdk.SandboxInfo, 0, len(h.sandboxes))
	for _, s := range h.sandboxes {
		out = append(out, s)
	}
	return out, nil
}

func (h *fakeHost) SandboxCreate(_ context.Context, req extsdk.CreateSandboxRequest) (extsdk.SandboxInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.createdSeq++
	id := fmt.Sprintf("sbx-%d", h.createdSeq)
	h.createReqs = append(h.createReqs, req)
	info := extsdk.SandboxInfo{
		ID:     id,
		Name:   req.Name,
		Image:  req.Image,
		Status: "running",
	}
	h.sandboxes[id] = info
	return info, nil
}

func (h *fakeHost) SandboxStop(_ context.Context, id string) (extsdk.SandboxInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info := h.sandboxes[id]
	info.Status = "stopped"
	h.sandboxes[id] = info
	return info, nil
}

func (h *fakeHost) SandboxDelete(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sandboxes, id)
	h.deletedIDs = append(h.deletedIDs, id)
	return nil
}

func (h *fakeHost) SandboxLogsFollow(ctx context.Context, id string, _ int, onLine func(string) error) error {
	if id == "sbx-1" {
		_ = onLine("√ Connected to GitHub")
		_ = onLine("2026-11-25 10:00:00Z: Listening for Jobs")
		_ = onLine("2026-11-25 10:00:01Z: Running job: test-suite")
		_ = onLine("token leak test ghp_super_secret_pat_123 jit-secret-token-1")
		_ = onLine("2026-11-25 10:00:02Z: Job test-suite completed with result: Failed")
		// Simulate ephemeral runner process exit.
		h.mu.Lock()
		sb := h.sandboxes[id]
		sb.Status = "stopped"
		h.sandboxes[id] = sb
		h.mu.Unlock()
		return nil
	}
	// Second (replenished) sandbox stays in standby until cancelled.
	_ = onLine("2026-11-25 10:00:03Z: Listening for Jobs")
	<-ctx.Done()
	return ctx.Err()
}

func TestStandbyEphemeralRunnerLifecycleAndReplenish(t *testing.T) {
	ctx := context.Background()
	st := state.NewStore(nil)
	red := mask.New()
	host := newFakeHost()
	gh := &fakeGH{}

	cfg, err := st.SaveRunner(ctx, state.RunnerConfig{
		Name:        "demo-runner",
		Scope:       "repo",
		Target:      "octocat/demo",
		AuthMode:    "pat",
		Labels:      []string{"self-hosted", "linux", "arm64", "gvx"},
		Concurrency: 1,
		AllowDocker: true,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveRunner: %v", err)
	}

	mgr := NewManager(st, host, gh, red, nil)
	mgr.pollInterval = 20 * time.Millisecond
	mgr.replenishDelay = 20 * time.Millisecond
	mgr.retryBackoff = 50 * time.Millisecond

	mgr.Reconcile()

	// Wait until sbx-1 finishes, gets deleted, history records failure, and sbx-2 replenishes standby.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		created := len(host.createReqs)
		deleted := len(host.deletedIDs)
		host.mu.Unlock()
		hist := st.History()
		if created >= 2 && deleted >= 1 && len(hist) == 1 && hist[0].Status == "failure" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	host.mu.Lock()
	if len(host.createReqs) < 2 {
		t.Fatalf("expected replenishment sandbox creation (>=2), got %d", len(host.createReqs))
	}
	if !host.createReqs[0].Ephemeral || !host.createReqs[0].MountDockerSock {
		t.Fatalf("expected Ephemeral=true and MountDockerSock=true, got %+v", host.createReqs[0])
	}
	if len(host.deletedIDs) < 1 || host.deletedIDs[0] != "sbx-1" {
		t.Fatalf("expected sbx-1 deleted after job exit, deletedIDs=%v", host.deletedIDs)
	}
	host.mu.Unlock()

	hist := st.History()
	if len(hist) != 1 || hist[0].Kind != "runner" || hist[0].JobID != "test-suite" || hist[0].Status != "failure" {
		t.Fatalf("unexpected runner history: %+v", hist)
	}
	logs := strings.Join(hist[0].Logs, "\n")
	if strings.Contains(logs, "ghp_super_secret_pat_123") || strings.Contains(logs, "jit-secret-token-1") {
		t.Fatalf("secret leaked in runner logs: %s", logs)
	}
	if !strings.Contains(logs, "***") {
		t.Fatalf("expected masked placeholder in logs: %s", logs)
	}

	// Shutdown must clean up replenished standby sandbox sbx-2.
	mgr.Shutdown(ctx)
	host.mu.Lock()
	remaining := len(host.sandboxes)
	host.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected all ephemeral sandboxes cleaned up on shutdown, remaining=%d", remaining)
	}
	_ = cfg
}
