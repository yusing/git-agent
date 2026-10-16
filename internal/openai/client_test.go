package openai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateEmbeddingsSendsDimensions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var payload struct {
			Input      []string `json:"input"`
			Model      string   `json:"model"`
			Dimensions int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "text-embedding-3-small" {
			t.Fatalf("model = %q", payload.Model)
		}
		if payload.Dimensions != 1024 {
			t.Fatalf("dimensions = %d", payload.Dimensions)
		}
		if len(payload.Input) != 2 || payload.Input[0] != "alpha" || payload.Input[1] != "beta" {
			t.Fatalf("input = %#v", payload.Input)
		}
		data := make([]map[string]any, len(payload.Input))
		for i := range payload.Input {
			data[i] = map[string]any{
				"object":    "embedding",
				"index":     i,
				"embedding": testEmbeddingVector(payload.Dimensions),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  payload.Model,
			"data":   data,
			"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	resp, err := NewHTTPClient(server.Client()).CreateEmbeddings(t.Context(), EmbeddingRequest{
		Model:      "text-embedding-3-small",
		Dimensions: 1024,
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Inputs:     []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dimensions != 1024 {
		t.Fatalf("response dimensions = %d", resp.Dimensions)
	}
	if len(resp.Vectors) != 2 || len(resp.Vectors[0]) != 1024 || len(resp.Vectors[1]) != 1024 {
		t.Fatalf("vectors = %#v", resp.Vectors)
	}
}

func TestNewHTTPClientInstallsBoundedDialTransport(t *testing.T) {
	t.Parallel()

	raw := &http.Client{Timeout: time.Minute}
	client := NewHTTPClient(raw).HTTPClient

	if client == raw {
		t.Fatal("NewHTTPClient should clone the supplied client before adding transport defaults")
	}
	if raw.Transport != nil {
		t.Fatal("NewHTTPClient mutated the supplied client")
	}
	if client.Timeout != time.Minute {
		t.Fatalf("Timeout = %v, want %v", client.Timeout, time.Minute)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil")
	}
}

func TestNewHTTPClientPreservesCustomTransport(t *testing.T) {
	t.Parallel()

	custom := &errorRoundTripper{err: errors.New("boom")}
	client := NewHTTPClient(&http.Client{Transport: custom}).HTTPClient

	if client.Transport != custom {
		t.Fatalf("Transport = %#v, want custom transport", client.Transport)
	}
}

type errorRoundTripper struct {
	err error
}

func (r *errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.err
}

func testEmbeddingVector(dimensions int) []float64 {
	vector := make([]float64, dimensions)
	if dimensions > 0 {
		vector[0] = 1
	}
	return vector
}
