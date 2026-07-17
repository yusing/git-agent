package search

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSearchFilePreservesContentsAcrossSizeHints(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		sizeHint int
	}{
		{name: "empty", content: "", sizeHint: 0},
		{name: "exact hint", content: "searchable content\n", sizeHint: len("searchable content\n")},
		{name: "file grew after stat", content: strings.Repeat("growth\n", 1024), sizeHint: 1},
		{name: "file shrank after stat", content: "short\n", sizeHint: MaxFileBytes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "input.futuretext", tt.content)

			buffer, err := readSearchFile(filepath.Join(root, "input.futuretext"), tt.sizeHint)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(buffer.data); got != tt.content {
				buffer.release()
				t.Fatalf("content = %q, want %q", got, tt.content)
			}
			buffer.release()
			if buffer.data != nil {
				t.Fatal("released buffer retained its byte slice")
			}
		})
	}
}

func TestReadSearchFileRejectsGrowthPastLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "input.futuretext", strings.Repeat("x", MaxFileBytes+1))

	buffer, err := readSearchFile(filepath.Join(root, "input.futuretext"), 1)
	if !errors.Is(err, errSearchFileOversized) {
		buffer.release()
		t.Fatalf("error = %v, want %v", err, errSearchFileOversized)
	}
}
