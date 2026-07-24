package background

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/trace"
)

const (
	testTaskID       = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	secondTestTaskID = "BCDEFGHIJKLMNOPQRSTUVWXYZA"
)

func TestStorePersistsFollowUpMetadata(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	metadata := TurnMetadata{ParentID: secondTestTaskID, Mode: "staged"}
	if err := store.AttachTurn(testTaskID, metadata); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(testTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != recordVersion || record.Turn == nil || *record.Turn != metadata {
		t.Fatalf("record = %#v", record)
	}
}

func TestStoreWaitsForAndRepeatsFinalReports(t *testing.T) {
	for _, command := range []string{"review", "simplify"} {
		t.Run(command, func(t *testing.T) {
			store := newTestStore(t)
			store.pollInterval = time.Nanosecond
			started := time.Now().UTC()
			if err := store.Create(testTaskID, command, 42, started); err != nil {
				t.Fatal(err)
			}
			var completeErr error
			var once sync.Once
			store.processAlive = func(int) bool {
				once.Do(func() {
					completeErr = store.Complete(testTaskID, trace.Event{
						Seq:   9,
						At:    started.Add(time.Second),
						Kind:  "final",
						Value: map[string]any{"text": map[string]any{"summary": "done"}},
					}, nil, started.Add(time.Second))
				})
				return true
			}
			for range 2 {
				report, err := store.Wait(t.Context(), testTaskID, command)
				if err != nil {
					t.Fatal(err)
				}
				if got := report.(map[string]any)["summary"]; got != "done" {
					t.Fatalf("report summary = %#v", got)
				}
			}
			if completeErr != nil {
				t.Fatal(completeErr)
			}
		})
	}
}

func TestStoreReturnsStoredErrorsAndRejectsWrongKind(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Wait(t.Context(), testTaskID, "simplify"); err == nil || !strings.Contains(err.Error(), "belongs to review") {
		t.Fatalf("wrong-kind error = %v", err)
	}
	if err := store.Complete(testTaskID, trace.Event{
		Seq: 2, At: now.Add(time.Second), Kind: "error", Value: map[string]any{"message": "provider unavailable"},
	}, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Wait(t.Context(), testTaskID, "review"); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("stored error = %v", err)
	}
}

func TestStorePersistsBoundedFailureDiagnostics(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	fingerprint := gitctx.ChangeFingerprint{BaseTree: strings.Repeat("a", 40), TargetTree: strings.Repeat("b", 40)}
	diagnostic := &FailureDiagnostic{
		Model: "gpt-test", Mode: "staged", MaxSteps: 60, MaxToolCalls: 48,
		RepositoryFingerprint: &fingerprint,
	}
	for seq := 1; seq <= maxDiagnosticEvents+2; seq++ {
		kind := "tool-call"
		value := map[string]any{
			"name": "read_file", "call_id": fmt.Sprintf("call_%d", seq),
			"arguments": strings.Repeat("argument", maxDiagnosticBytes),
		}
		if seq%2 == 0 {
			kind = "tool-output"
			value = map[string]any{
				"name": "read_file", "call_id": fmt.Sprintf("call_%d", seq),
				"content": strings.Repeat("repository content\n", maxDiagnosticLines+20),
			}
		}
		diagnostic.RecordToolEvent(trace.Event{Seq: seq, Kind: kind, Value: value})
	}
	if err := store.Complete(testTaskID, trace.Event{
		Seq: 20, At: now.Add(time.Second), Kind: "error", Value: map[string]any{"message": "provider unavailable"},
	}, diagnostic, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	record, err := store.Read(testTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != recordVersion || record.Failure == nil || record.Failure.Model != "gpt-test" {
		t.Fatalf("failure record = %#v", record)
	}
	if len(record.Failure.ToolEvents) != maxDiagnosticEvents || record.Failure.ToolEvents[0].Seq != 3 {
		t.Fatalf("diagnostic events = %#v", record.Failure.ToolEvents)
	}
	for _, event := range record.Failure.ToolEvents {
		if len(event.Payload) > maxDiagnosticBytes+256 || !event.Truncated {
			t.Fatalf("unbounded diagnostic event = %#v", event)
		}
		if strings.Contains(event.Payload, "repository content") || strings.Contains(event.Payload, "argumentargument") {
			t.Fatalf("diagnostic retained raw tool payload: %#v", event)
		}
	}
}

func TestStorePersistsWrappedBranchToolDiagnostics(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	diagnostic := &FailureDiagnostic{}
	diagnostic.RecordToolEvent(trace.Event{
		Seq: 3, Kind: "branch.event",
		Value: map[string]any{"event": map[string]any{
			"kind":  "future-event",
			"value": map[string]any{"kind": "tool-call", "arguments": `{"path":"collision.go"}`},
		}},
	})
	diagnostic.RecordToolEvent(trace.Event{
		Seq: 4, Kind: "branch.event",
		Value: map[string]any{"event": map[string]any{"kind": "tool-call", "value": "malformed"}},
	})
	diagnostic.RecordToolEvent(trace.Event{
		Seq: 7, Kind: "branch.event",
		Value: map[string]any{"event": map[string]any{
			"kind": "tool-call",
			"value": map[string]any{
				"name": "read_file", "call_id": "child-call", "arguments": `{"path":"internal/child.go","secret":"omitted"}`,
			},
		}},
	})
	diagnostic.RecordToolEvent(trace.Event{
		Seq: 9, Kind: "branch.event",
		Value: map[string]any{"event": map[string]any{
			"kind": "tool-output",
			"value": map[string]any{
				"name": "read_file", "call_id": "child-call",
				"content": `{"ok":false,"tool":"read_file","error":"missing"}`,
			},
		}},
	})
	if err := store.Complete(testTaskID, trace.Event{
		Seq: 10, At: now.Add(time.Second), Kind: "error", Value: map[string]any{"message": "child failed"},
	}, diagnostic, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	record, err := store.Read(testTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Failure == nil || len(record.Failure.ToolEvents) != 2 {
		t.Fatalf("failure diagnostic = %#v", record.Failure)
	}
	call, output := record.Failure.ToolEvents[0], record.Failure.ToolEvents[1]
	if call.Seq != 7 || call.Kind != "tool-call" || call.Tool != "read_file" || call.CallID != "child-call" {
		t.Fatalf("wrapped call = %#v", call)
	}
	if output.Seq != 9 || output.Kind != "tool-output" || !strings.Contains(output.Payload, `"error":"missing"`) {
		t.Fatalf("wrapped output = %#v", output)
	}
	if strings.Contains(call.Payload, "secret") {
		t.Fatalf("wrapped call retained unsafe argument: %#v", call)
	}
}

func TestStoreReadsLegacyRecordsWithoutDiagnostics(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	record := Record{
		Version: legacyRecordVersion, ID: testTaskID, Command: "review", PID: 42,
		StartedAt: now, UpdatedAt: now,
		Terminal: &trace.Event{Seq: 1, At: now, Kind: "final", Value: map[string]any{"text": map[string]any{"summary": "legacy"}}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, testTaskID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := store.Wait(t.Context(), testTaskID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["summary"] != "legacy" {
		t.Fatalf("legacy result = %#v", result)
	}
}

func TestStoreRejectsDeadProducerAndHonorsCancellation(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	store.processAlive = func(int) bool { return false }
	if _, err := store.Wait(t.Context(), testTaskID, "review"); err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("dead-producer error = %v", err)
	}

	waiting := make(chan struct{})
	var once sync.Once
	store.processAlive = func(int) bool {
		once.Do(func() { close(waiting) })
		return true
	}
	store.pollInterval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := store.Wait(ctx, testTaskID, "review")
		result <- err
	}()
	<-waiting
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestStoreRejectsLivePIDWithStaleHeartbeat(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	started := now.Add(-heartbeatStaleAfter - time.Second)
	if err := store.Create(testTaskID, "review", 42, started); err != nil {
		t.Fatal(err)
	}
	store.processAlive = func(int) bool { return true }
	store.now = func() time.Time { return now }
	if _, err := store.Wait(t.Context(), testTaskID, "review"); err == nil || !strings.Contains(err.Error(), "heartbeat is stale") {
		t.Fatalf("stale-heartbeat error = %v", err)
	}

	if err := store.touch(testTaskID, now); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(testTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.UpdatedAt.Equal(now) {
		t.Fatalf("heartbeat timestamp = %s, want %s", record.UpdatedAt, now)
	}
}

func TestStoreRejectsInvalidUnknownCorruptAndConflictingRecords(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create("../escape", "review", 42, now); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if _, err := store.Read(testTaskID); err == nil || !strings.Contains(err.Error(), "unknown background task") {
		t.Fatalf("unknown error = %v", err)
	}
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(testTaskID, "review", 42, now); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflict error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, secondTestTaskID+".json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(secondTestTaskID); err == nil || !strings.Contains(err.Error(), "decode background task") {
		t.Fatalf("corrupt error = %v", err)
	}
}

func TestFindStoreReturnsUnknownWhenMetadataRootDoesNotExist(t *testing.T) {
	_, err := FindStore(filepath.Join(t.TempDir(), "missing"), testTaskID)
	if err == nil || !strings.Contains(err.Error(), "unknown background task") {
		t.Fatalf("error = %v, want unknown background task", err)
	}
}

func TestStorePublishesAtomicPrivateRecordSnapshots(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.Create(testTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.dir, testTaskID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("record permissions = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory permissions = %04o, want 0700", got)
	}

	start := make(chan struct{})
	ready := make(chan error, 4)
	done := make(chan struct{})
	errCh := make(chan error, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			<-start
			if _, err := store.Read(testTaskID); err != nil {
				ready <- err
				return
			}
			ready <- nil
			for {
				select {
				case <-done:
					return
				default:
				}
				record, err := store.Read(testTaskID)
				if err != nil {
					errCh <- err
					return
				}
				if record.Terminal != nil && record.Terminal.Kind != "final" {
					errCh <- errors.New("reader observed invalid terminal state")
					return
				}
			}
		})
	}
	close(start)
	for range 4 {
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Complete(testTaskID, trace.Event{
		Seq: 2, At: now.Add(time.Second), Kind: "final", Value: map[string]any{"text": map[string]any{"summary": "done"}},
	}, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	close(done)
	readers.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
