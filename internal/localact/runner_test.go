package localact

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
)

func TestDockerShimPrecreatesVolumesRewritesNetworkAndTracksID(t *testing.T) {
	var (
		mu             sync.Mutex
		createdVolumes = map[string]bool{}
		gotNetworkMode string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripAPIVersion(r.URL.Path)
		switch {
		case r.Method == http.MethodPost && p == "/volumes/create":
			var req struct {
				Name string `json:"Name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			createdVolumes[req.Name] = true
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Name": req.Name})

		case r.Method == http.MethodPost && p == "/containers/create":
			var req struct {
				HostConfig struct {
					NetworkMode string `json:"NetworkMode"`
					Mounts      []struct {
						Type   string `json:"Type"`
						Source string `json:"Source"`
					} `json:"Mounts"`
				} `json:"HostConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			gotNetworkMode = req.HostConfig.NetworkMode
			for _, m := range req.HostConfig.Mounts {
				if m.Type == "volume" && !createdVolumes[m.Source] {
					mu.Unlock()
					http.Error(w, `{"message":"volume not owned"}`, http.StatusForbidden)
					return
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ctr-1234567890ab"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	var trackedName, trackedID string
	shim, err := StartDockerShim(upstream.URL, func(name, id string) {
		trackedName, trackedID = name, id
	})
	if err != nil {
		t.Fatalf("StartDockerShim: %v", err)
	}
	defer shim.Close()

	dialFn, err := dialerForDockerHost(shim.DockerHost())
	if err != nil {
		t.Fatalf("dialerForDockerHost: %v", err)
	}
	cl := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialFn(ctx)
			},
		},
	}

	// Send act-style POST /v1.52/containers/create with host network + uncreated named volumes.
	body := `{"Image":"catthehacker/ubuntu:act-latest","HostConfig":{"NetworkMode":"host","Mounts":[{"Type":"volume","Source":"act-toolcache","Target":"/opt/hostedtoolcache"},{"Type":"volume","Source":"act-job-env","Target":"/var/run/act"}]}}`
	resp, err := cl.Post("http://docker/v1.52/containers/create?name=act-ci-test", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /containers/create: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (%s)", resp.StatusCode, raw)
	}
	if trackedName != "act-ci-test" || trackedID != "ctr-1234567890ab" {
		t.Fatalf("tracked = (%q, %q)", trackedName, trackedID)
	}
	mu.Lock()
	if gotNetworkMode != "bridge" {
		t.Fatalf("expected NetworkMode rewritten from host -> bridge, got %q", gotNetworkMode)
	}
	if !createdVolumes["act-toolcache"] || !createdVolumes["act-job-env"] {
		t.Fatalf("expected volumes pre-created, got %+v", createdVolumes)
	}
	mu.Unlock()

	// Verify docker:// step with container:<job> network mode gets rewritten safely.
	stepBody := `{"Image":"alpine:3.21","HostConfig":{"NetworkMode":"container:act-ci-test"}}`
	resp2, err := cl.Post("http://docker/v1.52/containers/create?name=act-step", "application/json", bytes.NewBufferString(stepBody))
	if err != nil {
		t.Fatalf("POST step /containers/create: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("step status = %d", resp2.StatusCode)
	}
	mu.Lock()
	if gotNetworkMode != "bridge" {
		t.Fatalf("expected container: network mode rewritten to bridge, got %q", gotNetworkMode)
	}
	mu.Unlock()
}

func TestLocalManagerDryRunRecordsStepsAndMasksSecrets(t *testing.T) {
	repo := t.TempDir()
	wfDir := filepath.Join(repo, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wf := `name: Demo CI
on: push
jobs:
  build:
    name: Build App
    runs-on: ubuntu-latest
    steps:
      - name: Print secret
        run: echo "secret is ${{ secrets.MY_SECRET }}"
`
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.NewStore(nil)
	red := mask.New()
	mgr := NewManager(st, red, t.TempDir(), "", nil)
	defer mgr.Close()

	rec, err := mgr.StartRun(context.Background(), RunRequest{
		WorkspacePath: repo,
		WorkflowFile:  "ci.yml",
		Event:         "push",
		Secrets:       map[string]string{"MY_SECRET": "super-secret-value-xyz"},
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("StartRun dryrun: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur, ok := st.RunByID(rec.ID)
		if ok && cur.Status != "running" {
			if cur.Status != "success" {
				t.Fatalf("expected dryrun success, got %s (err=%q, logs=%v)", cur.Status, cur.Error, cur.Logs)
			}
			joined := strings.Join(cur.Logs, "\n")
			if strings.Contains(joined, "super-secret-value-xyz") {
				t.Fatalf("secret leaked in dryrun logs: %s", joined)
			}
			if len(cur.Steps) == 0 {
				t.Fatalf("expected recorded steps in dryrun")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for dryrun workflow to finish")
}
