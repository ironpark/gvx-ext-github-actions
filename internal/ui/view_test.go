package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/localact"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/runners"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
)

func TestRenderAllTabsAndDialogsProduceValidJSON(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	wfDir := filepath.Join(repo, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wf := `name: Build & Test
on:
  push:
  workflow_dispatch:
    inputs:
      target:
        description: Build target
        default: all
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo lint
  test:
    needs: lint
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.NewStore(nil)
	st.SetHostInfo(extsdk.HostInfo{OS: "darwin", Arch: "arm64", DockerMode: "sandbox", DockerReady: true})
	_, _ = st.AddWorkspace(ctx, repo)
	rc, _ := st.SaveRunner(ctx, state.RunnerConfig{
		Name:        "gvx-demo",
		Scope:       "repo",
		Target:      "octocat/" + filepath.Base(repo),
		Labels:      state.DefaultArchLabels("arm64"),
		Concurrency: 1,
		AllowDocker: true,
		Enabled:     true,
	})
	st.SetRunnerSlots(rc.ID, []state.RunnerLiveSlot{
		{SlotIndex: 1, SandboxID: "sbx-1", Name: "gvx-demo-1", Status: "standby", StartedAt: time.Now()},
	})
	runnerRun := st.AddRun(&state.RunRecord{
		Kind:       "runner",
		Status:     "failure",
		Target:     "octocat/" + filepath.Base(repo),
		RunnerID:   rc.ID,
		RunnerName: rc.Name,
		JobID:      "test",
		SandboxID:  "sbx-1",
		Logs:       []string{"step failed", "exit code 1"},
	})

	red := mask.New()
	localMgr := localact.NewManager(st, red, t.TempDir(), "", nil)
	defer localMgr.Close()
	runnerMgr := runners.NewManager(st, nil, nil, red, nil)
	ctrl := NewController(st, localMgr, runnerMgr, red, nil)

	uiBuilder := &extsdk.UI{}

	cases := []struct {
		name   string
		tab    string
		dialog string
	}{
		{"tab_local", TabLocal, dialogNone},
		{"tab_local_secrets_dialog", TabLocal, dialogSecrets},
		{"tab_runners", TabRunners, dialogNone},
		{"tab_runners_add_dialog", TabRunners, dialogAddRunner},
		{"tab_history", TabHistory, dialogNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl.mu.Lock()
			ctrl.activeTab = tc.tab
			ctrl.openDialog = tc.dialog
			ctrl.mu.Unlock()

			node := ctrl.Render(&extsdk.ViewContext{Context: &extsdk.Context{Locale: "en"}, ViewID: "main"}, uiBuilder)
			raw, err := json.Marshal(node)
			if err != nil {
				t.Fatalf("marshal %s: %v", tc.name, err)
			}
			if len(raw) == 0 || !strings.Contains(string(raw), `"kind":"column"`) {
				t.Fatalf("unexpected rendered tree: %s", raw)
			}
		})
	}

	// Verify 1-click "Reproduce Locally with act" maps runner failure to local workspace & job.
	msg := ctrl.loadRunIntoLocalTab(LocaleEnglish, runnerRun)
	if !strings.Contains(msg, "ci.yml") || !strings.Contains(msg, "test") {
		t.Fatalf("unexpected reproduce message: %q", msg)
	}
	ctrl.mu.Lock()
	if ctrl.activeTab != TabLocal || ctrl.selectedWorkflow != "ci.yml" || ctrl.selectedJob != "test" {
		t.Fatalf("reproduce state mismatch: tab=%q wf=%q job=%q", ctrl.activeTab, ctrl.selectedWorkflow, ctrl.selectedJob)
	}
	ctrl.mu.Unlock()
}

func TestAppHandlersCommandsAndToolsEndToEnd(t *testing.T) {
	repo := t.TempDir()
	wfDir := filepath.Join(repo, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wf := `name: Deploy
on:
  workflow_dispatch:
    inputs:
      target:
        description: Release target
        default: staging
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - run: echo deploy
`
	if err := os.WriteFile(filepath.Join(wfDir, "deploy.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu              sync.Mutex
		kv              = map[string]string{}
		secrets         = map[string]string{}
		openedTermType  string
		openedTermID    string
		pickedGestureID string
	)

	mockHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/host/info":
			_ = json.NewEncoder(w).Encode(extsdk.HostInfo{OS: "darwin", Arch: "arm64", DockerMode: "sandbox", DockerReady: true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/settings":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_act_image": state.DefaultActImage, "default_concurrency": 2})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/secrets":
			keys := make([]string, 0, len(secrets))
			for k := range secrets {
				keys = append(keys, k)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/secrets/"):
			k := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
			var body struct {
				Value string `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			secrets[k] = body.Value
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/secrets/"):
			k := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
			v, ok := secrets[k]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/secrets/"):
			k := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
			delete(secrets, k)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/storage/"):
			k := strings.TrimPrefix(r.URL.Path, "/v1/storage/")
			var body struct {
				Value string `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			kv[k] = body.Value
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/storage/"):
			k := strings.TrimPrefix(r.URL.Path, "/v1/storage/")
			v, ok := kv[k]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ui/pick-folder":
			pickedGestureID = r.Header.Get("X-GVX-Gesture-ID")
			_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": false, "path": repo})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ui/open-terminal":
			var req extsdk.OpenTerminalRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			openedTermType = req.TargetType
			openedTermID = req.TargetID
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && (r.URL.Path == "/v1/ui/toast" || r.URL.Path == "/v1/ui/badge" || r.URL.Path == "/v1/ui/copy"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockHost.Close()

	hostClient, err := extsdk.NewHost(mockHost.URL, "test-token")
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	st := state.NewStore(hostClient)
	red := mask.New()
	localMgr := localact.NewManager(st, red, t.TempDir(), "", nil)
	defer localMgr.Close()
	runnerMgr := runners.NewManager(st, nil, nil, red, nil)

	app := extsdk.New().SetVersion("0.1.0")
	ctrl := NewController(st, localMgr, runnerMgr, red, func() { app.NotifyChanged("main") })
	ctrl.SetBadgeHost(hostClient)
	ctrl.Register(app)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.RunWithConfig(ctx, extsdk.Config{
			ExtID:      "github-actions",
			Version:    "0.1.0",
			HostAPI:    mockHost.URL,
			HostToken:  "test-token",
			ListenAddr: "127.0.0.1:0",
			ExtToken:   "ext-secret",
		})
	}()

	var baseURL string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := app.ListenAddr(); addr != "" {
			baseURL = "http://" + addr
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if baseURL == "" {
		t.Fatal("extension app failed to start listener")
	}

	doReq := func(method, path string, body any, gesture string) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, baseURL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ext-secret")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if gesture != "" {
			req.Header.Set("X-GVX-Gesture-ID", gesture)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	// Open a real SSE view stream so extsdk refreshes its viewIndex on NotifyChanged.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, baseURL+"/v1/views/main/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	sseReq.Header.Set("Authorization", "Bearer ext-secret")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer sseResp.Body.Close()
	sseFrames := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				select {
				case sseFrames <- strings.TrimPrefix(line, "data: "):
				default:
				}
			}
		}
	}()
	// Wait for initial tree frame.
	select {
	case frame := <-sseFrames:
		if !strings.Contains(frame, `"kind":"column"`) {
			t.Fatalf("unexpected initial SSE tree: %s", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial SSE frame")
	}

	// 1. Health check
	code, raw := doReq(http.MethodGet, "/v1/health", nil, "")
	if code != http.StatusOK || !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("health failed: %d %s", code, raw)
	}

	// 2. Pick repository (verifies gesture token forwarding + workflow discovery + storage save)
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "pick_repo",
	}, "gesture-pick-1")
	if code != http.StatusOK {
		t.Fatalf("pick_repo failed: %d %s", code, raw)
	}
	mu.Lock()
	if pickedGestureID != "gesture-pick-1" {
		t.Fatalf("expected gesture token forwarded to pick-folder, got %q", pickedGestureID)
	}
	mu.Unlock()

	// 3. Toggle bind_workdir (boolean value via resolveViewMessage / wrapViewHandler)
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "toggle_bind_workdir",
		"kind":    "change",
		"value":   "true",
		"args":    []any{true},
	}, "g2")
	if code != http.StatusOK {
		t.Fatalf("toggle_bind_workdir failed: %d %s", code, raw)
	}
	ctrl.mu.Lock()
	if !ctrl.bindWorkdir {
		t.Fatalf("expected bindWorkdir=true")
	}
	ctrl.mu.Unlock()

	// 4. Set workflow_dispatch input (static arg + dynamic value)
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "set_dispatch_input",
		"kind":    "change",
		"value":   "production",
		"args":    []any{"target", "production"},
	}, "g3")
	if code != http.StatusOK {
		t.Fatalf("set_dispatch_input failed: %d %s", code, raw)
	}
	ctrl.mu.Lock()
	if ctrl.dispatchInputs["target"] != "production" {
		t.Fatalf("expected dispatchInputs[target]=production, got %q", ctrl.dispatchInputs["target"])
	}
	ctrl.mu.Unlock()

	// 5. Save and delete secret via Vault
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "open_secrets",
	}, "g4")
	if code != http.StatusOK {
		t.Fatalf("open_secrets failed: %d %s", code, raw)
	}
	// Wait for SSE stream to emit the updated tree containing dlg_secrets so viewIndex refreshes.
	waitSSE := time.NewTimer(3 * time.Second)
	for {
		select {
		case frame := <-sseFrames:
			if strings.Contains(frame, "dlg_secrets") {
				waitSSE.Stop()
				goto secretsReady
			}
		case <-waitSSE.C:
			t.Fatal("timed out waiting for dlg_secrets SSE frame")
		}
	}
secretsReady:
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "save_secret",
		"values": map[string]string{
			"/dlg_secrets/#0/secret_name":  "DEPLOY_KEY",
			"/dlg_secrets/#0/secret_value": "top-secret-deploy-key",
		},
	}, "g5")
	if code != http.StatusOK {
		t.Fatalf("save_secret failed: %d %s", code, raw)
	}
	if red.Mask("key=top-secret-deploy-key") != "key=***" {
		t.Fatalf("expected saved secret registered in redactor")
	}
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "delete_secret",
		"args":    []any{"DEPLOY_KEY"},
	}, "g6")
	if code != http.StatusOK {
		t.Fatalf("delete_secret failed: %d %s", code, raw)
	}

	// 6. Open Shell (2 static args: target_type, target_id)
	code, raw = doReq(http.MethodPost, "/v1/views/main/event", map[string]any{
		"handler": "open_shell",
		"args":    []any{"sandbox", "sbx-live-99"},
	}, "g7")
	if code != http.StatusOK {
		t.Fatalf("open_shell failed: %d %s", code, raw)
	}
	mu.Lock()
	if openedTermType != "sandbox" || openedTermID != "sbx-live-99" {
		t.Fatalf("open_shell forwarded (%q, %q)", openedTermType, openedTermID)
	}
	mu.Unlock()

	// 7. Command & MCP Tool endpoints
	code, raw = doReq(http.MethodPost, "/v1/commands/add-runner", map[string]any{"locale": "en"}, "g8")
	if code != http.StatusOK {
		t.Fatalf("command add-runner failed: %d %s", code, raw)
	}
	ctrl.mu.Lock()
	if ctrl.activeTab != TabRunners || ctrl.openDialog != dialogAddRunner {
		t.Fatalf("command add-runner state: tab=%q dialog=%q", ctrl.activeTab, ctrl.openDialog)
	}
	ctrl.mu.Unlock()

	code, raw = doReq(http.MethodPost, "/v1/tools/list_runners", map[string]any{"args": map[string]any{}}, "")
	if code != http.StatusOK || !strings.Contains(string(raw), `"runners"`) {
		t.Fatalf("tool list_runners failed: %d %s", code, raw)
	}

	// 8. Graceful shutdown
	sseCancel()
	code, raw = doReq(http.MethodPost, "/v1/shutdown", nil, "")
	if code != http.StatusOK {
		t.Fatalf("shutdown failed: %d %s", code, raw)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("app.RunWithConfig returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for app shutdown")
	}
}
