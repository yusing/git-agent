package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/trace"
)

type agentEventServer struct {
	server   *http.Server
	endpoint localHTTPEndpoint
	token    string

	mu     sync.Mutex
	events []trace.Event
	notify chan struct{}
	closed bool
}

func startAgentEventServer() (*agentEventServer, error) {
	token := rand.Text()
	listener, endpoint, err := listenLocalHTTP("/events", url.Values{"token": []string{token}})
	if err != nil {
		return nil, err
	}
	events := &agentEventServer{
		endpoint: endpoint,
		token:    token,
		notify:   make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", events.handleEvents)
	events.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		err := events.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			events.Finish()
		}
	}()
	return events, nil
}

func startDetachedAgentEventServer(stderr io.Writer, command, taskID string) (*agentEventServer, error) {
	events, err := startAgentEventServer()
	if err != nil {
		return nil, err
	}
	if err := writeDetachedLaunch(stderr, detachedLaunch{
		Command:  command,
		ID:       taskID,
		PID:      os.Getpid(),
		Endpoint: events.Endpoint(),
	}); err != nil {
		events.Close()
		return nil, err
	}
	if file, ok := stderr.(*os.File); ok {
		_ = file.Close()
	}
	return events, nil
}

func (s *agentEventServer) Endpoint() localHTTPEndpoint { return s.endpoint }

func (s *agentEventServer) Publish(event trace.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("agent event stream is closed")
	}
	s.events = append(s.events, event)
	close(s.notify)
	s.notify = make(chan struct{})
	return nil
}

func (s *agentEventServer) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.notify)
}

func (s *agentEventServer) Close() {
	s.Finish()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}

func (s *agentEventServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	cursor, err := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		events, notify, closed := s.after(cursor)
		for _, event := range events {
			data, err := sonic.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Kind, data); err != nil {
				return
			}
			cursor = event.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if closed {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-notify:
		}
	}
}

func (s *agentEventServer) after(cursor int) ([]trace.Event, <-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := 0
	for index < len(s.events) && s.events[index].Seq <= cursor {
		index++
	}
	events := append([]trace.Event(nil), s.events[index:]...)
	return events, s.notify, s.closed
}

func parseLastEventID(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	id, err := strconv.Atoi(value)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("invalid Last-Event-ID %q", value)
	}
	return id, nil
}
