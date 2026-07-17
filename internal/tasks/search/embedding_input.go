package search

import (
	"strings"
	"unicode/utf8"
	"unsafe"
)

type pooledEmbeddingInputs struct {
	texts   []string
	buffers [][]byte
}

func buildEmbeddingInput(chunk Chunk, text string, maxChars int) []byte {
	maxChars = normalizedEmbeddingMaxChars(maxChars)
	sizeHint := embeddingInputSizeHint(chunk, text, maxChars)
	data := recyclableBytes.GetAtLeast(sizeHint)
	remaining := maxChars
	appendText := func(text string) {
		data = appendCappedEmbeddingText(data, text, &remaining)
	}

	appendText("path: ")
	appendText(chunk.Path)
	appendText("\n")
	if chunk.Symbol != nil && chunk.Symbol.Name != "" {
		appendText("symbol: ")
		appendText(chunk.Symbol.Type)
		appendText(" ")
		appendText(chunk.Symbol.Name)
		appendText("\n")
	}
	if lang := languageForPath(chunk.Path); lang != "" {
		appendText("language: ")
		appendText(lang)
		appendText("\n")
	}
	appendText("\n")

	text = normalizeChunkBody(text)
	if text == "" || remaining == 0 {
		return data
	}
	first := true
	for line := range strings.SplitSeq(text, "\n") {
		if !first {
			appendText("\n")
		}
		first = false
		appendText(clampEmbeddingLine(line))
		if remaining == 0 {
			break
		}
	}
	return data
}

func embeddingInputSizeHint(chunk Chunk, text string, maxChars int) int {
	if chunk.embeddingInputHash != "" && chunk.embeddingMaxChars == maxChars {
		return chunk.embeddingInputBytes
	}
	headerBytes := len("path: \n\n") + len(chunk.Path)
	if chunk.Symbol != nil && chunk.Symbol.Name != "" {
		headerBytes += len("symbol:  \n") + len(chunk.Symbol.Type) + len(chunk.Symbol.Name)
	}
	if lang := languageForPath(chunk.Path); lang != "" {
		headerBytes += len("language: \n") + len(lang)
	}
	totalBytes := headerBytes + len(text)
	maxInt := int(^uint(0) >> 1)
	maxInputBytes := maxInt
	if maxChars <= maxInt/utf8.UTFMax {
		maxInputBytes = maxChars * utf8.UTFMax
	}
	return min(totalBytes, maxInputBytes)
}

func appendCappedEmbeddingText(dst []byte, text string, remaining *int) []byte {
	if text == "" || *remaining == 0 {
		return dst
	}
	if len(text) <= *remaining {
		*remaining -= utf8.RuneCountInString(text)
		return append(dst, text...)
	}

	chars := 0
	for end := range text {
		if chars == *remaining {
			*remaining = 0
			return append(dst, text[:end]...)
		}
		chars++
	}
	*remaining -= chars
	return append(dst, text...)
}

func (inputs *pooledEmbeddingInputs) release() {
	if inputs == nil {
		return
	}
	for i, buffer := range inputs.buffers {
		inputs.texts[i] = ""
		if buffer == nil {
			continue
		}
		recyclableBytes.Put(buffer)
	}
	inputs.texts = nil
	inputs.buffers = nil
}

func readOnlyString(buffer []byte) string {
	// Callers must retain buffer unchanged until the returned string is no
	// longer used. pooledEmbeddingInputs owns that lifetime through release.
	return unsafe.String(unsafe.SliceData(buffer), len(buffer))
}
