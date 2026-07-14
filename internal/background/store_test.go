package background

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/trace"
)

const (
	testTaskID       = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	secondTestTaskID = "BCDEFGHIJKLMNOPQRSTUVWXYZA"
)

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
					}, started.Add(time.Second))
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
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Wait(t.Context(), testTaskID, "review"); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("stored error = %v", err)
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
	}, now.Add(time.Second)); err != nil {
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
