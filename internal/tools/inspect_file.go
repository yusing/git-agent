package tools

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/gitctx"
)

const inspectMaxOutline = 200
const inspectMaxOutlineBytes = 64 << 10
const inspectMaxContent = 4 << 20

var jsonPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

type inspectFileTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

type fileOutlineEntry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Line int    `json:"line,omitempty"`
}

type outlineBuilder struct {
	entries   []fileOutlineEntry
	bytes     int
	truncated bool
}

func (b *outlineBuilder) add(entry fileOutlineEntry) bool {
	entryBytes := len(entry.Kind) + len(entry.Name)
	if len(b.entries) == inspectMaxOutline || entryBytes > inspectMaxOutlineBytes-b.bytes {
		b.truncated = true
		return false
	}
	b.entries = append(b.entries, entry)
	b.bytes += entryBytes
	return true
}

func (t inspectFileTool) Definition() Definition {
	return Definition{Name: "inspect_file", Description: "Report file size, line count, and a bounded structural outline without returning file content.", Schema: schema(map[string]any{
		"path":   stringProp("Repository-relative file path."),
		"source": fileSourceProp(t.mode),
	}, "path"), Strict: true}
}

func (t inspectFileTool) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path   string `json:"path"`
		Source string `json:"source"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		return Result{}, fmt.Errorf("path is required")
	}
	reader, source, err := openInspectedFile(t.repo, t.mode, args.Path, args.Source)
	if err != nil {
		return Result{}, err
	}
	defer reader.Close()
	kind := fileOutlineKind(args.Path)
	content, size, lines, contentTruncated, err := inspectContent(ctx, reader, kind != "none")
	if err != nil {
		return Result{}, err
	}

	outline, outlineTruncated := inspectOutline(kind, args.Path, content)
	return jsonResult("inspect_file", map[string]any{
		"path": args.Path, "source": source, "outline_kind": kind,
		"size_bytes": size, "lines": lines, "outline": outline,
	}, contentTruncated || outlineTruncated)
}

func inspectContent(ctx context.Context, reader io.Reader, retain bool) ([]byte, int64, int64, bool, error) {
	var content []byte
	if retain {
		content = make([]byte, 0, inspectMaxContent)
	}
	buffer := make([]byte, 32*1024)
	size, lines, lastByte := int64(0), int64(0), byte('\n')
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, false, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			size += int64(n)
			lines += int64(bytes.Count(chunk, []byte{'\n'}))
			lastByte = chunk[n-1]
			if retain && len(content) < inspectMaxContent {
				content = append(content, chunk[:min(n, inspectMaxContent-len(content))]...)
			}
		}
		if err != nil && err != io.EOF {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, 0, false, ctxErr
			}
			return nil, 0, 0, false, err
		}
		if err == io.EOF {
			break
		}
	}
	if size > 0 && lastByte != '\n' {
		lines++
	}
	return content, size, lines, retain && size > inspectMaxContent, nil
}

func fileOutlineKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "code"
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	default:
		return "none"
	}
}

func inspectOutline(kind, path string, content []byte) ([]fileOutlineEntry, bool) {
	switch kind {
	case "code":
		return goOutline(path, content)
	case "markdown":
		return markdownOutline(content)
	case "json":
		return jsonOutline(content)
	default:
		return nil, false
	}
}

func goOutline(path string, content []byte) ([]fileOutlineEntry, bool) {
	content, ok := validOutlineText(content)
	if !ok {
		return nil, false
	}
	set := token.NewFileSet()
	file, _ := parser.ParseFile(set, path, content, 0)
	if file == nil {
		return nil, false
	}
	var outline outlineBuilder
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			name := declaration.Name.Name
			kind := "function"
			if declaration.Recv != nil && len(declaration.Recv.List) > 0 {
				kind = "method"
				name = receiverName(declaration.Recv.List[0].Type) + "." + name
			}
			if !outline.add(fileOutlineEntry{Kind: kind, Name: name, Line: set.Position(declaration.Pos()).Line}) {
				return outline.entries, true
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				if typed, ok := spec.(*ast.TypeSpec); ok {
					if !outline.add(fileOutlineEntry{Kind: "type", Name: typed.Name.Name, Line: set.Position(typed.Pos()).Line}) {
						return outline.entries, true
					}
				}
			}
		}
	}
	return outline.entries, outline.truncated
}

func receiverName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverName(expression.X)
	case *ast.IndexExpr:
		return receiverName(expression.X)
	case *ast.IndexListExpr:
		return receiverName(expression.X)
	default:
		return "?"
	}
}

func markdownOutline(content []byte) ([]fileOutlineEntry, bool) {
	content, ok := validOutlineText(content)
	if !ok {
		return nil, false
	}
	var outline outlineBuilder
	var fence byte
	fenceLength := 0
	line := 0
	for raw := range bytes.Lines(content) {
		line++
		text := strings.TrimSuffix(string(raw), "\n")
		trimmed := strings.TrimLeft(text, " ")
		if len(text)-len(trimmed) > 3 {
			continue
		}
		markerLength := 0
		if len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			for markerLength < len(trimmed) && trimmed[markerLength] == trimmed[0] {
				markerLength++
			}
		}
		if markerLength >= 3 {
			if fence == 0 {
				fence, fenceLength = trimmed[0], markerLength
			} else if trimmed[0] == fence && markerLength >= fenceLength && strings.TrimSpace(trimmed[markerLength:]) == "" {
				fence, fenceLength = 0, 0
			}
			continue
		}
		if fence != 0 {
			continue
		}
		level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		if level > 0 && level <= 6 && len(trimmed) > level && trimmed[level] == ' ' {
			if !outline.add(fileOutlineEntry{Kind: "heading" + strconv.Itoa(level), Name: strings.TrimSpace(trimmed[level:]), Line: line}) {
				return outline.entries, true
			}
		}
	}
	return outline.entries, outline.truncated
}

func jsonOutline(content []byte) ([]fileOutlineEntry, bool) {
	content, ok := validOutlineText(content)
	if !ok {
		return nil, false
	}
	var value any
	if err := sonic.ConfigStd.Unmarshal(content, &value); err != nil {
		return nil, false
	}
	var outline outlineBuilder
	var walk func(any, string)
	walk = func(value any, pointer string) {
		if outline.truncated {
			return
		}
		switch value := value.(type) {
		case map[string]any:
			names := make([]string, 0, len(value))
			for name := range value {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				child := value[name]
				next := pointer + "/" + jsonPointerEscaper.Replace(name)
				if !outline.add(fileOutlineEntry{Kind: jsonValueKind(child), Name: next}) {
					return
				}
				walk(child, next)
			}
		case []any:
			for index, child := range value {
				next := pointer + "/" + strconv.Itoa(index)
				if !outline.add(fileOutlineEntry{Kind: jsonValueKind(child), Name: next}) {
					return
				}
				walk(child, next)
			}
		}
	}
	walk(value, "")
	return outline.entries, outline.truncated
}

func validOutlineText(content []byte) ([]byte, bool) {
	if utf8.Valid(content) {
		return content, !bytes.ContainsRune(content, 0)
	}
	for trim := 1; trim < utf8.UTFMax && trim < len(content); trim++ {
		prefix := content[:len(content)-trim]
		if utf8.Valid(prefix) {
			return prefix, !bytes.ContainsRune(prefix, 0)
		}
	}
	return nil, false
}

func jsonValueKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
