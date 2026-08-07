package cli

import (
	"maps"
	"sync"
	"time"

	"github.com/yusing/git-agent/internal/trace"
)

type explorePhaseTrace struct {
	started  time.Time
	recorder *trace.Recorder
	mu       sync.Mutex
	writeErr error
}

func newExplorePhaseTrace(started time.Time, recorder *trace.Recorder) *explorePhaseTrace {
	return &explorePhaseTrace{started: started, recorder: recorder}
}

func (t *explorePhaseTrace) record(phase string, duration time.Duration, fields map[string]any) {
	if t == nil || t.recorder == nil {
		return
	}
	value := make(map[string]any, len(fields)+3)
	maps.Copy(value, fields)
	value["phase"] = phase
	value["duration_ms"] = duration.Milliseconds()
	value["elapsed_ms"] = time.Since(t.started).Milliseconds()
	if err := t.recorder.Write("explore.phase", value); err != nil {
		t.mu.Lock()
		if t.writeErr == nil {
			t.writeErr = err
		}
		t.mu.Unlock()
	}
}

func (t *explorePhaseTrace) recordSimple(phase string, duration time.Duration) {
	t.record(phase, duration, nil)
}

func (t *explorePhaseTrace) err() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeErr
}
