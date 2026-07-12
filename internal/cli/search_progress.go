package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	searchtask "github.com/yusing/git-agent/internal/tasks/search"
)

type searchProgressAgent struct {
	server   *http.Server
	listener net.Listener

	mu       sync.RWMutex
	snapshot searchProgressSnapshot
}

type searchProgressSnapshot struct {
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	Reused    int       `json:"reused"`
	Percent   float64   `json:"percent"`
	ElapsedMS int64     `json:"elapsed_ms"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}

func startSearchProgressAgent() (*searchProgressAgent, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	agent := &searchProgressAgent{
		listener: listener,
		snapshot: searchProgressSnapshot{
			Status:    "waiting",
			UpdatedAt: time.Now().UTC(),
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /progress", agent.handleProgress)
	agent.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		err := agent.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			agent.Fail(err)
		}
	}()
	return agent, nil
}

func (a *searchProgressAgent) URL() string {
	return "http://" + a.listener.Addr().String() + "/progress"
}

func (a *searchProgressAgent) Update(progress searchtask.Progress) {
	a.mu.Lock()
	defer a.mu.Unlock()

	status := progress.Status
	if status == "" {
		status = "indexing"
	}
	percent := 0.0
	if progress.Total > 0 {
		percent = float64(progress.Done) / float64(progress.Total) * 100
	}
	if status == "indexing" && progress.Total > 0 && progress.Done >= progress.Total {
		status = "done"
		percent = 100
	}
	a.snapshot = searchProgressSnapshot{
		Status:    status,
		Detail:    progress.Detail,
		Done:      progress.Done,
		Total:     progress.Total,
		Reused:    progress.Reused,
		Percent:   percent,
		ElapsedMS: progress.Elapsed.Milliseconds(),
		UpdatedAt: time.Now().UTC(),
	}
}

func (a *searchProgressAgent) Complete() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.snapshot.Status != "done" && a.snapshot.Status != "error" {
		a.snapshot.Status = "complete"
		a.snapshot.UpdatedAt = time.Now().UTC()
	}
}

func (a *searchProgressAgent) Fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.snapshot.Status = "error"
	a.snapshot.Error = err.Error()
	a.snapshot.UpdatedAt = time.Now().UTC()
}

func (a *searchProgressAgent) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = a.server.Shutdown(ctx)
}

func (a *searchProgressAgent) handleProgress(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	snapshot := a.snapshot
	a.mu.RUnlock()

	data, err := sonic.Marshal(snapshot)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}
