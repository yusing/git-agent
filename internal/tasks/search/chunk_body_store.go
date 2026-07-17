package search

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type chunkBodyRef struct {
	store  *chunkBodyStore
	offset int64
	length int
}

type chunkBodyStore struct {
	file   *os.File
	path   string
	size   int64
	closed bool
}

func newChunkBodyStore() (*chunkBodyStore, error) {
	file, err := os.CreateTemp("", "git-agent-search-chunks-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary search chunk store: %w", err)
	}
	path := file.Name()
	if err := os.Remove(path); err == nil {
		path = ""
	}
	return &chunkBodyStore{file: file, path: path}, nil
}

func (s *chunkBodyStore) close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	closeErr := s.file.Close()
	if s.path == "" {
		return closeErr
	}
	return errors.Join(closeErr, os.Remove(s.path))
}

func (s *chunkBodyStore) spill(fileText string, chunks []Chunk) error {
	needsBody := false
	for i := range chunks {
		if !chunks[i].pathOnly {
			needsBody = true
			break
		}
	}
	if !needsBody {
		for i := range chunks {
			chunks[i].text = ""
		}
		return nil
	}

	text := normalizeChunkBody(fileText)
	lineStarts := []int{0}
	for i := range len(text) {
		if text[i] == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	for i := range chunks {
		chunk := &chunks[i]
		if chunk.pathOnly {
			continue
		}
		if chunk.StartLine < 1 || chunk.EndLine < chunk.StartLine || chunk.EndLine > len(lineStarts) {
			return fmt.Errorf("chunk body range %s:%d-%d exceeds %d lines", chunk.Path, chunk.StartLine, chunk.EndLine, len(lineStarts))
		}
		start := lineStarts[chunk.StartLine-1]
		end := len(text)
		if chunk.EndLine < len(lineStarts) {
			end = lineStarts[chunk.EndLine] - 1
		}
		if text[start:end] != chunk.text {
			return fmt.Errorf("chunk body range mismatch for %s:%d-%d", chunk.Path, chunk.StartLine, chunk.EndLine)
		}
		chunk.body = chunkBodyRef{store: s, offset: s.size + int64(start), length: end - start}
	}

	written, err := io.WriteString(s.file, text)
	if err != nil {
		return fmt.Errorf("write temporary search chunk store: %w", err)
	}
	if written != len(text) {
		return fmt.Errorf("write temporary search chunk store: %w", io.ErrShortWrite)
	}
	s.size += int64(written)
	for i := range chunks {
		if chunks[i].pathOnly {
			chunks[i].text = ""
			continue
		}
		chunks[i].text = ""
	}
	return nil
}

type loadedChunkBody struct {
	text   string
	buffer []byte
}

func loadChunkBodyBuffer(chunk Chunk) (loadedChunkBody, error) {
	if chunk.body.store == nil {
		return loadedChunkBody{text: chunk.text}, nil
	}
	if chunk.body.length == 0 {
		return loadedChunkBody{}, nil
	}
	body := recyclableBytes.GetAtLeast(chunk.body.length)
	body = body[:chunk.body.length]
	reader := io.NewSectionReader(chunk.body.store.file, chunk.body.offset, int64(chunk.body.length))
	written, err := io.ReadFull(reader, body)
	if err != nil {
		recyclableBytes.Put(body)
		return loadedChunkBody{}, fmt.Errorf("read temporary search chunk %s:%d-%d: %w", chunk.Path, chunk.StartLine, chunk.EndLine, err)
	}
	if written != chunk.body.length {
		recyclableBytes.Put(body)
		return loadedChunkBody{}, fmt.Errorf("read temporary search chunk %s:%d-%d: %w", chunk.Path, chunk.StartLine, chunk.EndLine, io.ErrUnexpectedEOF)
	}
	return loadedChunkBody{text: readOnlyString(body), buffer: body}, nil
}

func (body *loadedChunkBody) release() {
	if body == nil || body.buffer == nil {
		return
	}
	body.text = ""
	recyclableBytes.Put(body.buffer)
	body.buffer = nil
}

func normalizeChunkBody(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.TrimSuffix(text, "\n")
}
