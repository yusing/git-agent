package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/textutil"
)

const (
	jqToolName              = "jq"
	jqMaxDocumentBytes      = 16 << 20
	jqMaxSelectedValueBytes = 64 << 10
	jqMaxSelectedValueLines = 2000
)

type jqTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

func (t jqTool) Definition() Definition {
	return Definition{
		Name: jqToolName,
		Description: "Retrieve one value from a repository JSON file using a plain RFC 6901 JSON Pointer. " +
			"Use an empty pointer for the document root; jq filter expressions are not supported.",
		Schema: schema(map[string]any{
			"path":      stringProp("Repository-relative JSON file path."),
			"source":    fileSourceProp(t.mode),
			"pointer":   stringProp("Plain RFC 6901 JSON Pointer. Empty selects the document root."),
			"max_bytes": intProp("Maximum bytes of the formatted selected value to return.", 1, jqMaxSelectedValueBytes),
			"max_lines": intProp("Maximum lines of the formatted selected value to return.", 1, jqMaxSelectedValueLines),
		}, "path", "pointer"),
		Strict: true,
	}
}

func (t jqTool) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path     string `json:"path"`
		Source   string `json:"source"`
		Pointer  string `json:"pointer"`
		MaxBytes int    `json:"max_bytes"`
		MaxLines int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		return Result{}, errors.New("path is required")
	}
	if args.MaxBytes < 0 || args.MaxBytes > jqMaxSelectedValueBytes {
		return Result{}, fmt.Errorf("max_bytes must be between 1 and %d, or zero for the default", jqMaxSelectedValueBytes)
	}
	if args.MaxLines < 0 || args.MaxLines > jqMaxSelectedValueLines {
		return Result{}, fmt.Errorf("max_lines must be between 1 and %d, or zero for the default", jqMaxSelectedValueLines)
	}

	reader, source, err := openInspectedFile(t.repo, t.mode, args.Path, args.Source)
	if err != nil {
		return Result{}, err
	}
	defer reader.Close()

	document, err := readJQDocument(ctx, reader)
	if err != nil {
		return Result{}, fmt.Errorf("read JSON file %q: %w", args.Path, err)
	}
	root, err := decodeJQDocument(document)
	if err != nil {
		return Result{}, fmt.Errorf("parse JSON file %q: %w", args.Path, err)
	}
	value, err := resolveJSONPointer(root, args.Pointer)
	if err != nil {
		return Result{}, err
	}

	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode value at JSON pointer %q: %w", args.Pointer, err)
	}
	valueLines := 1 + bytes.Count(formatted, []byte{'\n'})
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	data := map[string]any{
		"path": args.Path, "source": source, "pointer": args.Pointer,
		"value_kind": jsonValueKind(value), "value_bytes": len(formatted), "value_lines": valueLines,
	}
	truncated := len(formatted) > maxBytes || valueLines > maxLines
	if truncated {
		preview, _ := textutil.Limit(string(formatted), maxBytes, maxLines)
		data["value_preview"] = preview
	} else {
		data["value"] = value
	}
	return jsonResult(jqToolName, data, truncated)
}

func readJQDocument(ctx context.Context, reader io.Reader) ([]byte, error) {
	document := make([]byte, 0, min(jqMaxDocumentBytes, 64<<10))
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			if len(document)+n > jqMaxDocumentBytes {
				return nil, fmt.Errorf("document exceeds %d-byte limit", jqMaxDocumentBytes)
			}
			document = append(document, buffer[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return document, nil
			}
			return nil, err
		}
	}
}

func decodeJQDocument(document []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	switch err := decoder.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return value, nil
	case err == nil:
		return nil, errors.New("document contains more than one JSON value")
	default:
		return nil, fmt.Errorf("invalid trailing data: %w", err)
	}
}

func resolveJSONPointer(root any, pointer string) (any, error) {
	tokens, err := parseJSONPointer(pointer)
	if err != nil {
		return nil, err
	}
	value := root
	for position, token := range tokens {
		switch current := value.(type) {
		case map[string]any:
			next, ok := current[token]
			if !ok {
				return nil, fmt.Errorf("JSON pointer %q: object key %q does not exist at token %d", pointer, token, position)
			}
			value = next
		case []any:
			index, err := parseJSONArrayIndex(token)
			if err != nil {
				return nil, fmt.Errorf("JSON pointer %q: token %d: %w", pointer, position, err)
			}
			if index >= len(current) {
				return nil, fmt.Errorf("JSON pointer %q: array index %d is out of range at token %d", pointer, index, position)
			}
			value = current[index]
		default:
			return nil, fmt.Errorf("JSON pointer %q: cannot apply token %q to %s value at token %d", pointer, token, jsonValueKind(current), position)
		}
	}
	return value, nil
}

func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if pointer[0] != '/' {
		return nil, fmt.Errorf("JSON pointer %q must be empty or start with /", pointer)
	}
	rawTokens := strings.Split(pointer[1:], "/")
	tokens := make([]string, len(rawTokens))
	for index, raw := range rawTokens {
		var token strings.Builder
		for offset := 0; offset < len(raw); offset++ {
			if raw[offset] != '~' {
				token.WriteByte(raw[offset])
				continue
			}
			if offset+1 == len(raw) || (raw[offset+1] != '0' && raw[offset+1] != '1') {
				return nil, fmt.Errorf("JSON pointer %q has invalid escape in token %d", pointer, index)
			}
			offset++
			if raw[offset] == '0' {
				token.WriteByte('~')
			} else {
				token.WriteByte('/')
			}
		}
		tokens[index] = token.String()
	}
	return tokens, nil
}

func parseJSONArrayIndex(token string) (int, error) {
	if token == "" {
		return 0, errors.New("array index is empty")
	}
	if len(token) > 1 && token[0] == '0' {
		return 0, fmt.Errorf("array index %q has a leading zero", token)
	}
	for _, digit := range token {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("array index %q is not a non-negative integer", token)
		}
	}
	index, err := strconv.Atoi(token)
	if err != nil {
		return 0, fmt.Errorf("array index %q is too large", token)
	}
	return index, nil
}
