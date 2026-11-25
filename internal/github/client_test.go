package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	o, r, err := ParseTarget("repo", "https://github.com/octocat/hello-world.git")
	if err != nil || o != "octocat" || r != "hello-world" {
		t.Fatalf("ParseTarget repo = (%q, %q, %v)", o, r, err)
	}
	o, r, err = ParseTarget("org", "my-org")
	if err != nil || o != "my-org" || r != "" {
		t.Fatalf("ParseTarget org = (%q, %q, %v)", o, r, err)
	}
	if _, _, err := ParseTarget("repo", "missing-slash"); err == nil {
		t.Fatal("expected error for invalid repo target")
	}
}

func TestGenerateJITConfigAndList(t *testing.T) {
	var gotPath, gotAuth string
	var gotReq JITConfigRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/generate-jitconfig"):
			_ = json.NewDecoder(r.Body).Decode(&gotReq)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(JITConfigResponse{
				Runner:           RemoteRunner{ID: 42, Name: gotReq.Name, Status: "offline"},
				EncodedJITConfig: "eyJ0ZXN0IjoidmFsdWUifQ==",
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runners"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runners": []RemoteRunner{{ID: 42, Name: "gvx-1", Status: "online"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cl := NewClient()
	cl.BaseURL = srv.URL

	resp, err := cl.GenerateJITConfig(context.Background(), "repo", "octocat/demo", "ghp_test123", JITConfigRequest{
		Name:   "gvx-demo-1",
		Labels: []string{"self-hosted", "linux", "arm64"},
	})
	if err != nil {
		t.Fatalf("GenerateJITConfig: %v", err)
	}
	if gotPath != "/repos/octocat/demo/actions/runners/generate-jitconfig" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer ghp_test123" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotReq.RunnerGroupID != 1 || resp.EncodedJITConfig != "eyJ0ZXN0IjoidmFsdWUifQ==" || resp.Runner.ID != 42 {
		t.Fatalf("unexpected req/resp: req=%+v resp=%+v", gotReq, resp)
	}

	runners, err := cl.ListRunners(context.Background(), "org", "acme", "ghp_test123")
	if err != nil || len(runners) != 1 || gotPath != "/orgs/acme/actions/runners" {
		t.Fatalf("ListRunners = %+v, err=%v, path=%q", runners, err, gotPath)
	}
}
