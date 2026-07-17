package search

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildEmbeddingInputMatchesReferenceFormatting(t *testing.T) {
	tests := []struct {
		name     string
		chunk    Chunk
		text     string
		maxChars int
	}{
		{
			name:     "path language and symbol",
			chunk:    Chunk{Path: "internal/search/task.go", Symbol: &Symbol{Type: "function", Name: "Run"}},
			text:     "first\nsecond\n",
			maxChars: DefaultEmbeddingMaxInputChars,
		},
		{
			name:     "unicode cap",
			chunk:    Chunk{Path: "notes.md"},
			text:     "世界🙂\nnext",
			maxChars: 19,
		},
		{
			name:     "invalid utf8",
			chunk:    Chunk{Path: "notes.txt"},
			text:     "valid\xfftail",
			maxChars: 16,
		},
		{
			name:     "line clamp and normalized newlines",
			chunk:    Chunk{Path: "notes.txt"},
			text:     strings.Repeat("x", maxEmbeddingLineChars+10) + "\r\nnext\r\n",
			maxChars: DefaultEmbeddingMaxInputChars,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := buildEmbeddingInput(tt.chunk, tt.text, tt.maxChars)
			got := string(input)
			recyclableBytes.Put(input)
			want := cappedEmbeddingInput(referenceEmbeddingText(tt.chunk, tt.text), tt.maxChars)
			if got != want {
				t.Fatalf("input = %q, want %q", got, want)
			}
		})
	}
}

func TestPooledEmbeddingInputsReleaseOwnership(t *testing.T) {
	chunk := Chunk{Path: "notes.txt", text: "searchable body"}
	prepareChunkEmbedding(&chunk, DefaultEmbeddingMaxInputChars)
	inputs, err := chunkEmbeddingInputs([]Chunk{chunk}, DefaultEmbeddingMaxInputChars)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.texts) != 1 || inputs.texts[0] != embeddingText(chunk, chunk.text) {
		t.Fatalf("inputs = %#v", inputs.texts)
	}
	inputs.release()
	if inputs.texts != nil || inputs.buffers != nil {
		t.Fatalf("released inputs retained ownership: %#v", inputs)
	}
}

func referenceEmbeddingText(chunk Chunk, text string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "path: %s\n", chunk.Path)
	if chunk.Symbol != nil && chunk.Symbol.Name != "" {
		fmt.Fprintf(&builder, "symbol: %s %s\n", chunk.Symbol.Type, chunk.Symbol.Name)
	}
	if lang := languageForPath(chunk.Path); lang != "" {
		fmt.Fprintf(&builder, "language: %s\n", lang)
	}
	builder.WriteByte('\n')
	for i, line := range splitLines(text) {
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(clampEmbeddingLine(line))
	}
	return builder.String()
}
