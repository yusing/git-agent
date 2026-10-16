package openai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type EmbeddingClient interface {
	CreateEmbeddings(context.Context, EmbeddingRequest) (EmbeddingResponse, error)
}

type EmbeddingRequest struct {
	Model      string
	Dimensions int
	BaseURL    string
	APIKey     string
	// Inputs are borrowed for the duration of CreateEmbeddings. Implementations
	// that retain them after returning must clone the strings.
	Inputs []string
}

type EmbeddingResponse struct {
	Model      string
	Vectors    [][]float64
	Dimensions int
}

type TextFormat struct {
	Name        string         `json:"name"`
	Schema      map[string]any `json:"schema"`
	Description string         `json:"description,omitempty"`
	Strict      bool           `json:"strict"`
}

type Item struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	RawJSON   string `json:"raw_json,omitempty"`
	SDKJSON   string `json:"sdk_json,omitempty"`
}

type Usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningTokens       int64 `json:"reasoning_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type SDKClient struct{ HTTPClient *http.Client }

const DefaultDialTimeout = 5 * time.Second

func NewHTTPClient(client *http.Client) *SDKClient {
	return &SDKClient{HTTPClient: withDefaultDialTimeout(client)}
}

func (c *SDKClient) CreateEmbeddings(ctx context.Context, request EmbeddingRequest) (EmbeddingResponse, error) {
	if len(request.Inputs) == 0 {
		return EmbeddingResponse{}, errors.New("embedding request requires input")
	}
	opts := []option.RequestOption{
		option.WithAPIKey(request.APIKey),
		option.WithBaseURL(request.BaseURL),
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(http.DefaultClient))
	}
	client := openaisdk.NewClient(opts...)

	params := openaisdk.EmbeddingNewParams{
		Model: openaisdk.EmbeddingModel(request.Model),
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: request.Inputs,
		},
		EncodingFormat: openaisdk.EmbeddingNewParamsEncodingFormatFloat,
	}
	if request.Dimensions > 0 {
		params.Dimensions = openaisdk.Int(int64(request.Dimensions))
	}
	response, err := client.Embeddings.New(ctx, params)
	if err != nil {
		return EmbeddingResponse{}, upstreamError(err)
	}
	if len(response.Data) != len(request.Inputs) {
		return EmbeddingResponse{}, fmt.Errorf("embedding response count = %d, want %d", len(response.Data), len(request.Inputs))
	}

	vectors := make([][]float64, len(response.Data))
	dimensions := 0
	for _, item := range response.Data {
		if item.Index < 0 || int(item.Index) >= len(vectors) {
			return EmbeddingResponse{}, fmt.Errorf("embedding response index %d out of range", item.Index)
		}
		vector := item.Embedding
		if len(vector) == 0 {
			return EmbeddingResponse{}, fmt.Errorf("embedding response index %d is empty", item.Index)
		}
		if dimensions == 0 {
			dimensions = len(vector)
		}
		if len(vector) != dimensions {
			return EmbeddingResponse{}, fmt.Errorf("embedding dimensions mismatch: %d and %d", dimensions, len(vector))
		}
		vectors[item.Index] = vector
	}
	for i, vector := range vectors {
		if vector == nil {
			return EmbeddingResponse{}, fmt.Errorf("embedding response missing index %d", i)
		}
	}
	return EmbeddingResponse{
		Model:      response.Model,
		Vectors:    vectors,
		Dimensions: dimensions,
	}, nil
}

func withDefaultDialTimeout(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	if cloned.Transport == nil {
		cloned.Transport = newTransportWithDialTimeout(DefaultDialTimeout)
	}
	return &cloned
}

func newTransportWithDialTimeout(timeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return transport
}

func upstreamError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("upstream request failed: %w", err)
}

func NewMessage(role, text string) Item { return Item{Type: "message", Role: role, Content: text} }
