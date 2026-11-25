package state

import (
	"context"
	"encoding/json"
	"testing"
)

type memKV struct {
	data map[string][]byte
}

func newMemKV() *memKV { return &memKV{data: map[string][]byte{}} }

func (m *memKV) StorageGetJSON(_ context.Context, key string, dst any) (bool, error) {
	b, ok := m.data[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, dst)
}

func (m *memKV) StorageSetJSON(_ context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.data[key] = b
	return nil
}

func TestStorePersistenceAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV()
	st := NewStore(kv)

	ws, err := st.AddWorkspace(ctx, "/tmp/demo-repo")
	if err != nil || ws.Name != "demo-repo" {
		t.Fatalf("AddWorkspace = %+v, %v", ws, err)
	}
	rc, err := st.SaveRunner(ctx, RunnerConfig{
		Scope:       "repo",
		Target:      "octocat/hello-world",
		Labels:      DefaultArchLabels("arm64"),
		Concurrency: 2,
		AllowDocker: true,
		Enabled:     true,
	})
	if err != nil || rc.ID == "" {
		t.Fatalf("SaveRunner = %+v, %v", rc, err)
	}

	run := st.AddRun(&RunRecord{
		Kind:         "local",
		Status:       "running",
		WorkflowFile: "ci.yml",
		Event:        "push",
	})
	st.AppendRunLog(run.ID, "hello line")
	if err := st.PersistHistory(ctx); err != nil {
		t.Fatalf("PersistHistory: %v", err)
	}

	// Reload in a fresh store: running run should be marked cancelled.
	st2 := NewStore(kv)
	if err := st2.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st2.Workspaces()) != 1 || st2.Workspaces()[0].Path != "/tmp/demo-repo" {
		t.Fatalf("reloaded workspaces = %+v", st2.Workspaces())
	}
	if len(st2.Runners()) != 1 || st2.Runners()[0].Target != "octocat/hello-world" {
		t.Fatalf("reloaded runners = %+v", st2.Runners())
	}
	hist := st2.History()
	if len(hist) != 1 || hist[0].Status != "cancelled" {
		t.Fatalf("reloaded history = %+v", hist)
	}
}

func TestAppendRunLogBoundsMemory(t *testing.T) {
	st := NewStore(nil)
	run := st.AddRun(&RunRecord{Kind: "local", Status: "running"})
	bigLine := string(make([]byte, 1024))
	for range 100 {
		st.AppendRunLog(run.ID, bigLine)
	}
	got, ok := st.RunByID(run.ID)
	if !ok {
		t.Fatal("run not found")
	}
	if len(got.Logs) > 30 {
		t.Fatalf("expected bounded log lines under 24 KiB cap, got %d lines", len(got.Logs))
	}
}
