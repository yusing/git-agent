package explore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testProjectID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDispositionLogFallsBackToHomeStateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	log, err := NewDispositionLog(testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "git-agent", testProjectID, "explore.log")
	if log.path != want {
		t.Fatalf("log path = %q, want %q", log.path, want)
	}
	if _, err := NewDispositionLog("../invalid"); err == nil {
		t.Fatal("invalid project ID succeeded")
	}
}

func TestDispositionLogRecordsBatchAndBranchFieldsPrivately(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	log, err := NewDispositionLog(testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(log.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log.path+".lock", nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(log.path+".lock", 0o666); err != nil {
		t.Fatal(err)
	}
	parent := &Session{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Depth: 1}
	items := []BatchItem{{ID: "BBBBBBBBBBBBBBBBBBBBBBBBBB"}, {ID: "CCCCCCCCCCCCCCCCCCCCCCCCCC"}}
	if err := log.AppendBatch(t.Context(), "batch-example", "/workspace/example", parent, items); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Lines(string(data))
	count := 0
	for line := range lines {
		count++
		for _, want := range []string{
			" mode=batched ", " branch=true ", " project_id=" + testProjectID + " ",
			` workspace="/workspace/example" `, " batch=batch-example ", " size=2 ",
			" parent=AAAAAAAAAAAAAAAAAAAAAAAAAA ", " depth=2 ", " query=[redacted]",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("log line missing %q: %s", want, line)
			}
		}
	}
	if count != 2 {
		t.Fatalf("log lines = %d, want 2", count)
	}
	for _, path := range []string{filepath.Dir(filepath.Dir(log.path)), filepath.Dir(log.path)} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory %s mode = %o, want 700", path, got)
		}
	}
	for _, path := range []string{log.path, log.path + ".lock"} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file %s mode = %o, want 600", path, got)
		}
	}
}

func TestDispositionLogSerializesConcurrentAppends(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	log, err := NewDispositionLog(testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for index := range 20 {
		wait.Go(func() {
			itemID := strings.Repeat(string(rune('A'+index)), 26)
			if appendErr := log.AppendBatch(context.WithoutCancel(t.Context()), "batch-"+itemID, "/workspace", nil, []BatchItem{{ID: itemID}}); appendErr != nil {
				t.Errorf("append disposition: %v", appendErr)
			}
		})
	}
	wait.Wait()
	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 20 {
		t.Fatalf("log lines = %d, want 20\n%s", got, data)
	}
}

func TestCoordinatorLogsSealedBatchBeforeRunnerFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	log, err := NewDispositionLog(testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := testCoordinator(testStore(t))
	coordinator.DispositionLog = log
	_, err = coordinator.Run(t.Context(), nil, "fails after sealing", func(context.Context) (Prepared, error) {
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}, func(context.Context, *Session, []BatchItem) (map[string]BatchResult, error) {
		return nil, os.ErrPermission
	})
	if err == nil {
		t.Fatal("runner failure succeeded")
	}
	data, readErr := os.ReadFile(log.path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if text := string(data); !strings.Contains(text, " mode=unbatched ") || !strings.Contains(text, " branch=false ") || !strings.Contains(text, " depth=0 ") {
		t.Fatalf("failure disposition = %q", text)
	}
}

func TestCoordinatorIgnoresDispositionLogFailure(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateHome, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	log, err := NewDispositionLog(testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := testCoordinator(testStore(t))
	coordinator.DispositionLog = log
	if output := runFresh(t, coordinator, "logging must stay non-fatal"); output.ID == "" {
		t.Fatal("explore returned an empty ID")
	}
}
