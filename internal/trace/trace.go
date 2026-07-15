package trace

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
)

type Recorder struct {
	eventWriter io.Writer
	eventSink   func(Event) error
	mu          sync.Mutex
	seq         int
}

type Event struct {
	Seq   int            `json:"seq"`
	At    time.Time      `json:"at"`
	Kind  string         `json:"kind"`
	Value map[string]any `json:"value"`
}

const (
	largeStringPreviewThreshold = 16 * 1024
	consoleStringPreviewBytes   = 1200
	consoleStringPreviewLines   = 20
	consoleInlineFields         = 6
	consoleInlineWidth          = 140
)

var sonicUseNumber = sonic.Config{UseNumber: true}.Froze()

func NewStream(command string, writer io.Writer) (*Recorder, error) {
	return startMemory(&Recorder{eventWriter: writer}, command)
}

func NewEventStream(command string, sink func(Event) error) (*Recorder, error) {
	if sink == nil {
		return nil, errors.New("event sink is required")
	}
	return startMemory(&Recorder{eventSink: sink}, command)
}

func startMemory(recorder *Recorder, command string) (*Recorder, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	now := time.Now().UTC()
	value := map[string]any{
		"id":         now.Format("20060102T150405.000000000Z") + "-" + command,
		"started_at": now.Format(time.RFC3339Nano),
		"command":    command,
	}
	if err := recorder.appendEventLocked("session.started", value, true); err != nil {
		return nil, err
	}
	return recorder, nil
}

func (r *Recorder) Write(kind string, value any) error {
	if r == nil {
		return nil
	}

	return r.writeMap(kind, normalizedMapValue(value, true), true)
}

// WriteExact preserves string fields exactly for machine-consumed streaming
// contracts. Unlike Write, it does not expand JSON-looking strings or compact
// large strings into previews.
func (r *Recorder) WriteExact(kind string, value any) error {
	if r == nil {
		return nil
	}

	return r.writeMap(kind, normalizedMapValue(value, false), false)
}

func (r *Recorder) WriteStructured(kind string, value map[string]any) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.writeMapLocked(kind, value, true)
}

func (r *Recorder) writeMap(kind string, value map[string]any, compact bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.writeMapLocked(kind, value, compact)
}

func (r *Recorder) writeMapLocked(kind string, value map[string]any, compact bool) error {
	return r.appendEventLocked(kind, value, compact)
}

func Normalize(value any) any {
	return normalizeValue(value, true)
}

func normalizeValue(value any, expandStrings bool) any {
	data, err := sonic.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if err := sonicUseNumber.Unmarshal(data, &normalized); err != nil {
		return value
	}
	if !expandStrings {
		return normalized
	}
	return expandJSONStrings(normalized)
}

func normalizedMapValue(value any, expandStrings bool) map[string]any {
	normalized := normalizeValue(value, expandStrings)
	if mapped, ok := normalized.(map[string]any); ok {
		return mapped
	}
	return map[string]any{"value": normalized}
}

func expandJSONStrings(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = expandJSONStrings(item)
		}
		return value
	case []any:
		for idx, item := range value {
			value[idx] = expandJSONStrings(item)
		}
		return value
	case string:
		return expandJSONString(value)
	default:
		return value
	}
}

func expandJSONString(value string) any {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) == 0 {
		return value
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return value
	}
	var expanded any
	if err := sonicUseNumber.UnmarshalFromString(trimmed, &expanded); err != nil {
		return value
	}
	return expandJSONStrings(expanded)
}

func (r *Recorder) appendEventLocked(kind string, value map[string]any, compact bool) error {
	r.seq++
	eventValue := maps.Clone(value)
	if compact {
		var err error
		eventValue, err = r.compactedMapValueLocked(value)
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	if r.eventWriter != nil {
		if err := writeConsoleTraceEvent(r.eventWriter, now, kind, eventValue); err != nil {
			return err
		}
	}
	if r.eventSink != nil {
		return r.eventSink(Event{Seq: r.seq, At: now, Kind: kind, Value: eventValue})
	}
	return nil
}

func writeConsoleTraceEvent(writer io.Writer, at time.Time, kind string, value map[string]any) error {
	return writeConsoleEvent(writer, at, kind, consoleEventValue(kind, value), consoleColorEnabled(writer))
}

// WriteConsoleDiagnostic writes a human console diagnostic event with the same
// renderer used by streaming traces.
func WriteConsoleDiagnostic(writer io.Writer, kind string, attrs ...slog.Attr) error {
	return writeConsoleAttrEvent(writer, time.Now().UTC(), kind, attrs, consoleColorEnabled(writer))
}

type consoleInlineField struct {
	key   string
	value string
}

type consoleBlockField struct {
	key  string
	text string
	meta string
}

func writeConsoleEvent(writer io.Writer, at time.Time, message string, fields map[string]any, color bool) error {
	inline, blocks := consoleFields(fields)
	return writeConsoleFieldsEvent(writer, at, message, inline, blocks, color)
}

func writeConsoleAttrEvent(writer io.Writer, at time.Time, message string, attrs []slog.Attr, color bool) error {
	inline, blocks := consoleAttrFields(attrs)
	return writeConsoleFieldsEvent(writer, at, message, inline, blocks, color)
}

func writeConsoleFieldsEvent(writer io.Writer, at time.Time, message string, inline []consoleInlineField, blocks []consoleBlockField, color bool) error {
	var out strings.Builder
	writeConsoleHeader(&out, at, message, color)
	if len(blocks) == 0 && len(inline) <= consoleInlineFields && inlineWidth(inline) <= consoleInlineWidth {
		writeConsoleInlineFields(&out, inline, color)
		out.WriteByte('\n')
	} else {
		out.WriteByte('\n')
		writeConsoleWrappedFields(&out, inline, color)
	}
	for _, block := range blocks {
		writeConsoleBlock(&out, block, color)
	}
	_, err := io.WriteString(writer, out.String())
	return err
}

func writeConsoleHeader(out *strings.Builder, at time.Time, message string, color bool) {
	out.WriteString(consoleColorize(at.Local().Format("15:04:05"), consoleColorDim, color))
	out.WriteByte(' ')
	out.WriteString(consoleColorize("INF", consoleColorGreen, color))
	if message != "" {
		out.WriteByte(' ')
		out.WriteString(consoleColorize(message, consoleColorBold, color))
	}
}

func writeConsoleInlineFields(out *strings.Builder, fields []consoleInlineField, color bool) {
	for _, field := range fields {
		out.WriteByte(' ')
		writeConsoleInlineField(out, field, color)
	}
}

func writeConsoleWrappedFields(out *strings.Builder, fields []consoleInlineField, color bool) {
	if len(fields) == 0 {
		return
	}
	lineWidth := 0
	out.WriteString("  ")
	for idx, field := range fields {
		itemWidth := len(field.key) + 1 + len(field.value)
		if idx > 0 {
			if lineWidth+1+itemWidth > consoleInlineWidth {
				out.WriteByte('\n')
				out.WriteString("  ")
				lineWidth = 0
			} else {
				out.WriteByte(' ')
				lineWidth++
			}
		}
		writeConsoleInlineField(out, field, color)
		lineWidth += itemWidth
	}
	out.WriteByte('\n')
}

func writeConsoleInlineField(out *strings.Builder, field consoleInlineField, color bool) {
	out.WriteString(consoleColorize(field.key, consoleColorKey, color))
	out.WriteByte('=')
	out.WriteString(field.value)
}

func writeConsoleBlock(out *strings.Builder, block consoleBlockField, color bool) {
	out.WriteString("  ")
	out.WriteString(consoleColorize(block.key, consoleColorKey, color))
	out.WriteString(":\n")
	for line := range strings.SplitSeq(block.text, "\n") {
		out.WriteString("    ")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if block.meta != "" {
		out.WriteString("    ")
		out.WriteString(consoleColorize(block.meta, consoleColorDim, color))
		out.WriteByte('\n')
	}
}

func consoleColorize(value, colorCode string, enabled bool) string {
	if !enabled || colorCode == "" {
		return value
	}
	return colorCode + value + consoleColorReset
}

func consoleFields(fields map[string]any) ([]consoleInlineField, []consoleBlockField) {
	var inline []consoleInlineField
	var blocks []consoleBlockField
	collectConsoleFields("", fields, &inline, &blocks)
	return inline, blocks
}

func consoleAttrFields(attrs []slog.Attr) ([]consoleInlineField, []consoleBlockField) {
	var inline []consoleInlineField
	var blocks []consoleBlockField
	for _, attr := range attrs {
		collectConsoleAttr("", attr, &inline, &blocks)
	}
	return inline, blocks
}

func collectConsoleAttr(prefix string, attr slog.Attr, inline *[]consoleInlineField, blocks *[]consoleBlockField) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	value := attr.Value.Resolve()
	key := joinConsoleKey(prefix, attr.Key)
	if value.Kind() == slog.KindGroup {
		for _, child := range value.Group() {
			collectConsoleAttr(key, child, inline, blocks)
		}
		return
	}
	collectConsoleSlogValue(key, value, inline, blocks)
}

func collectConsoleSlogValue(prefix string, value slog.Value, inline *[]consoleInlineField, blocks *[]consoleBlockField) {
	switch value.Kind() {
	case slog.KindAny:
		collectConsoleFields(prefix, value.Any(), inline, blocks)
	case slog.KindString:
		text := value.String()
		if strings.Contains(text, "\n") || len(text) > consoleStringPreviewBytes {
			preview, truncated := multilinePreview(text, consoleStringPreviewBytes, consoleStringPreviewLines)
			*blocks = append(*blocks, consoleBlockField{
				key:  prefix,
				text: preview,
				meta: consolePreviewMeta(map[string]any{
					"bytes":     len(text),
					"lines":     lineCount(text),
					"truncated": truncated,
				}),
			})
			return
		}
		*inline = append(*inline, consoleInlineField{key: prefix, value: formatConsoleScalar(text)})
	default:
		*inline = append(*inline, consoleInlineField{key: prefix, value: value.String()})
	}
}

func collectConsoleFields(prefix string, value any, inline *[]consoleInlineField, blocks *[]consoleBlockField) {
	switch value := value.(type) {
	case map[string]any:
		if previewMap(value) {
			*blocks = append(*blocks, consolePreviewBlock(prefix, value))
			return
		}
		for _, key := range slices.Sorted(maps.Keys(value)) {
			collectConsoleFields(joinConsoleKey(prefix, key), value[key], inline, blocks)
		}
	case []any:
		if consoleScalarSlice(value) {
			*inline = append(*inline, consoleInlineField{key: prefix, value: formatConsoleSlice(value)})
			return
		}
		*inline = append(*inline, consoleInlineField{key: prefix + ".items", value: strconv.Itoa(len(value))})
	case string:
		if strings.Contains(value, "\n") || len(value) > consoleStringPreviewBytes {
			preview, truncated := multilinePreview(value, consoleStringPreviewBytes, consoleStringPreviewLines)
			*blocks = append(*blocks, consoleBlockField{
				key:  prefix,
				text: preview,
				meta: consolePreviewMeta(map[string]any{
					"bytes":     len(value),
					"lines":     lineCount(value),
					"truncated": truncated,
				}),
			})
			return
		}
		*inline = append(*inline, consoleInlineField{key: prefix, value: formatConsoleScalar(value)})
	default:
		*inline = append(*inline, consoleInlineField{key: prefix, value: formatConsoleScalar(value)})
	}
}

func consolePreviewBlock(key string, value map[string]any) consoleBlockField {
	return consoleBlockField{
		key:  key,
		text: fmt.Sprint(value["preview"]),
		meta: consolePreviewMeta(value),
	}
}

func consolePreviewMeta(value map[string]any) string {
	var parts []string
	if raw, ok := value["bytes"]; ok {
		parts = append(parts, fmt.Sprintf("%v bytes", raw))
	}
	if raw, ok := value["lines"]; ok {
		parts = append(parts, fmt.Sprintf("%v lines", raw))
	}
	prefix := ""
	if truncated, _ := value["truncated"].(bool); truncated {
		prefix = "… truncated"
	}
	if prefix == "" && len(parts) == 0 {
		return ""
	}
	if prefix == "" {
		return "(" + strings.Join(parts, ", ") + ")"
	}
	if len(parts) == 0 {
		return prefix
	}
	return prefix + " (" + strings.Join(parts, ", ") + ")"
}

func joinConsoleKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func inlineWidth(fields []consoleInlineField) int {
	var width int
	for idx, field := range fields {
		if idx > 0 {
			width++
		}
		width += len(field.key) + 1 + len(field.value)
	}
	return width
}

func formatConsoleSlice(value []any) string {
	parts := make([]string, len(value))
	for idx, item := range value {
		parts[idx] = formatConsoleScalar(item)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func formatConsoleScalar(value any) string {
	switch value := value.(type) {
	case nil:
		return "<nil>"
	case string:
		if value == "" || strings.ContainsAny(value, " \t\r\n\"=") {
			return strconv.Quote(value)
		}
		return value
	case time.Duration:
		return value.String()
	case time.Time:
		return value.Local().Format(time.RFC3339)
	default:
		return fmt.Sprint(value)
	}
}

func consoleColorEnabled(writer io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	if forced := os.Getenv("CLICOLOR_FORCE"); forced != "" && forced != "0" {
		return true
	}
	if forced := os.Getenv("FORCE_COLOR"); forced != "" && forced != "0" {
		return true
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

const (
	consoleColorReset = "\x1b[0m"
	consoleColorBold  = "\x1b[1m"
	consoleColorDim   = "\x1b[2m"
	consoleColorKey   = "\x1b[36m"
	consoleColorGreen = "\x1b[32m"
)

func consoleMapValue(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = consoleValue(item)
	}
	return result
}

func consoleEventValue(kind string, value map[string]any) map[string]any {
	switch kind {
	case "session.started":
		return consolePick(value, "command")
	case "session":
		return consoleSessionValue(value)
	case "request":
		return consoleRequestValue(value)
	case "response":
		return consoleResponseValue(value)
	case "final":
		return consolePick(value, "text", "tool_calls", "repair_calls")
	case "error":
		return consolePick(value, "message", "generated_commit_message")
	default:
		return consoleMapValue(value)
	}
}

func consoleSessionValue(value map[string]any) map[string]any {
	result := consolePick(value, "command", "mode")
	if repo, ok := value["repo"].(map[string]any); ok {
		addConsoleValue(result, "branch", repo["branch"])
		if head, ok := repo["head_sha"].(string); ok && len(head) >= 12 {
			result["head"] = head[:12]
		}
	}
	if stagedPaths, ok := itemCount(value["staged_paths"]); ok {
		result["staged_paths"] = stagedPaths
	}
	if prepared, ok := value["prepared_commit_context"].(map[string]any); ok {
		if contextPack, ok := prepared["context_pack"].(map[string]any); ok {
			if overview, ok := contextPack["overview"].(map[string]any); ok {
				addConsoleValue(result, "changes", overview["summary"])
				addConsoleValue(result, "scope", overview["dominant_scope"])
			}
		}
	}
	return result
}

func consoleRequestValue(value map[string]any) map[string]any {
	result := consolePick(value, "model", "service_tier")
	if inputItems, ok := itemCount(value["input"]); ok {
		result["input_items"] = inputItems
	}
	if toolItems, ok := itemCount(value["tools"]); ok {
		result["tools"] = toolItems
	}
	return result
}

func consoleResponseValue(value map[string]any) map[string]any {
	result := consolePick(value, "id", "finish_kind", "text")
	raw, _ := value["raw_json"].(map[string]any)
	addConsoleValue(result, "model", raw["model"])
	addConsoleValue(result, "service_tier", raw["service_tier"])
	if usage, ok := raw["usage"].(map[string]any); ok {
		addConsoleValue(result, "input_tokens", usage["input_tokens"])
		addConsoleValue(result, "output_tokens", usage["output_tokens"])
		addConsoleValue(result, "total_tokens", usage["total_tokens"])
	}
	return result
}

func consolePick(value map[string]any, keys ...string) map[string]any {
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		addConsoleValue(result, key, value[key])
	}
	return result
}

func addConsoleValue(result map[string]any, key string, value any) {
	if value == nil {
		return
	}
	result[key] = consoleValue(value)
}

func itemCount(value any) (int, bool) {
	switch value := value.(type) {
	case []any:
		return len(value), true
	case []string:
		return len(value), true
	default:
		return 0, false
	}
}

func consoleValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if previewMap(value) {
			return value
		}
		return consoleMapValue(value)
	case []any:
		if consoleScalarSlice(value) {
			items := make([]any, len(value))
			for idx, item := range value {
				items[idx] = consoleValue(item)
			}
			return items
		}
		return map[string]any{"items": len(value)}
	case string:
		if len(value) <= consoleStringPreviewBytes && !strings.Contains(value, "\n") {
			return value
		}
		preview, truncated := multilinePreview(value, consoleStringPreviewBytes, consoleStringPreviewLines)
		result := map[string]any{
			"bytes":   len(value),
			"preview": preview,
		}
		if strings.Contains(value, "\n") {
			result["lines"] = lineCount(value)
			result["multiline"] = true
		}
		if truncated {
			result["truncated"] = true
		}
		return result
	default:
		return value
	}
}

func previewMap(value map[string]any) bool {
	_, hasBytes := value["bytes"]
	_, hasPreview := value["preview"]
	return hasBytes && hasPreview
}

func consoleScalarSlice(value []any) bool {
	if len(value) > 10 {
		return false
	}
	for _, item := range value {
		if !consoleScalar(item) {
			return false
		}
	}
	return true
}

func consoleScalar(value any) bool {
	switch value := value.(type) {
	case nil, bool, stdjson.Number, int, int64, float64:
		return true
	case string:
		return len(value) <= consoleStringPreviewBytes && !strings.Contains(value, "\n")
	default:
		return false
	}
}

func multilinePreview(value string, maxBytes, maxLines int) (string, bool) {
	preview := validPrefix(value, maxBytes)
	truncated := len(preview) < len(value)
	lines := strings.Split(preview, "\n")
	if len(lines) > maxLines {
		preview = strings.Join(lines[:maxLines], "\n")
		truncated = true
	}
	return strings.TrimRight(preview, "\n"), truncated
}

func validPrefix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	preview := value[:maxBytes]
	for !utf8.ValidString(preview) && len(preview) > 0 {
		preview = preview[:len(preview)-1]
	}
	return preview
}

func lineCount(value string) int {
	return strings.Count(value, "\n") + 1
}

func (r *Recorder) compactedMapValueLocked(value map[string]any) (map[string]any, error) {
	compacted, err := r.compactedValueLocked(value)
	if err != nil {
		return nil, err
	}
	mapped, ok := compacted.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compacted trace event normalized to %T, want object", compacted)
	}
	return mapped, nil
}

func (r *Recorder) compactedValueLocked(value any) (any, error) {
	data, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := sonicUseNumber.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	return r.compactLargeStringsLocked(normalized)
}

func (r *Recorder) compactLargeStringsLocked(value any) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			compacted, err := r.compactLargeStringsLocked(item)
			if err != nil {
				return nil, err
			}
			value[key] = compacted
		}
		return value, nil
	case []any:
		for idx, item := range value {
			compacted, err := r.compactLargeStringsLocked(item)
			if err != nil {
				return nil, err
			}
			value[idx] = compacted
		}
		return value, nil
	case string:
		if len(value) < largeStringPreviewThreshold {
			return value, nil
		}
		return inlineStringPreview(value), nil
	default:
		return value, nil
	}
}

func inlineStringPreview(value string) map[string]any {
	preview, _ := multilinePreview(value, consoleStringPreviewBytes, consoleStringPreviewLines)
	result := map[string]any{
		"bytes":     len(value),
		"preview":   preview,
		"truncated": true,
	}
	if strings.Contains(value, "\n") {
		result["lines"] = lineCount(value)
		result["multiline"] = true
	}
	return result
}
