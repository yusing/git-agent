package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
)

type Recorder struct {
	dir                string
	eventsPath         string
	eventWriter        io.Writer
	sessionPath        string
	inMemory           bool
	mu                 sync.Mutex
	seq                int
	snapshot           snapshot
	currentStep        int
	openToolPairByCall map[string]int
}

type snapshot struct {
	Version int              `json:"version"`
	Session map[string]any   `json:"session"`
	Static  map[string]any   `json:"static,omitempty"`
	Items   []map[string]any `json:"items,omitempty"`
	Steps   []step           `json:"steps,omitempty"`
	Final   map[string]any   `json:"final,omitempty"`
	Error   map[string]any   `json:"error,omitempty"`
}

type step struct {
	Step     int              `json:"step"`
	Request  *stepRequest     `json:"request,omitempty"`
	Response *stepResponse    `json:"response,omitempty"`
	Tools    []stepTool       `json:"tools,omitempty"`
	Budgets  []map[string]any `json:"budgets,omitempty"`
}

type stepTool struct {
	CallItem   int            `json:"call_item,omitempty"`
	OutputItem int            `json:"output_item,omitempty"`
	Trace      map[string]any `json:"trace,omitempty"`
}

type stepRequest struct {
	Model        any `json:"model,omitempty"`
	Instructions any `json:"instructions,omitempty"`
	InputEnd     int `json:"input_end"`
}

type stepResponse struct {
	ID         any   `json:"id,omitempty"`
	FinishKind any   `json:"finish_kind,omitempty"`
	Text       any   `json:"text,omitempty"`
	ToolCalls  []int `json:"tool_calls,omitempty"`
}

const (
	largeStringArtifactThreshold = 16 * 1024
	artifactPreviewBytes         = 1200
	consoleStringPreviewBytes    = 1200
	consoleStringPreviewLines    = 20
	consoleInlineFields          = 6
	consoleInlineWidth           = 140
)

var sonicUseNumber = sonic.Config{UseNumber: true}.Froze()

func New(root, command string) (*Recorder, error) {
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405.000000000Z")
	sessionID := stamp + "-" + command
	dir := filepath.Join(root, ".git-agent", "sessions", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	recorder := &Recorder{
		dir:         dir,
		eventsPath:  filepath.Join(dir, "events.ndjson"),
		sessionPath: filepath.Join(dir, "session.json"),
		snapshot: snapshot{
			Version: 2,
			Session: map[string]any{
				"id":         sessionID,
				"started_at": now.Format(time.RFC3339Nano),
				"command":    command,
			},
			Static: map[string]any{},
		},
		currentStep:        -1,
		openToolPairByCall: map[string]int{},
	}
	if err := recorder.appendEventLocked("session.started", maps.Clone(recorder.snapshot.Session)); err != nil {
		return nil, err
	}
	if err := recorder.writeSnapshotLocked(); err != nil {
		return nil, err
	}
	return recorder, nil
}

func newMemory(command string) *Recorder {
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405.000000000Z")
	sessionID := stamp + "-" + command
	return &Recorder{
		inMemory: true,
		snapshot: snapshot{
			Version: 2,
			Session: map[string]any{
				"id":         sessionID,
				"started_at": now.Format(time.RFC3339Nano),
				"command":    command,
			},
			Static: map[string]any{},
		},
		currentStep:        -1,
		openToolPairByCall: map[string]int{},
	}
}

func NewStream(command string, writer io.Writer) (*Recorder, error) {
	recorder := newMemory(command)
	recorder.eventWriter = writer
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.appendEventLocked("session.started", maps.Clone(recorder.snapshot.Session)); err != nil {
		return nil, err
	}
	return recorder, nil
}

func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

func (r *Recorder) Write(kind string, value any) error {
	if r == nil {
		return nil
	}

	return r.writeMap(kind, normalizeMapValue(value))
}

func (r *Recorder) WriteStructured(kind string, value map[string]any) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.writeMapLocked(kind, value)
}

func (r *Recorder) writeMap(kind string, value map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.writeMapLocked(kind, value)
}

func (r *Recorder) writeMapLocked(kind string, value map[string]any) error {
	if !r.inMemory || r.eventWriter != nil {
		if err := r.appendEventLocked(kind, value); err != nil {
			return err
		}
	}
	if err := r.applyLocked(kind, value); err != nil {
		return err
	}
	if r.inMemory {
		return nil
	}
	return r.writeSnapshotLocked()
}

func Normalize(value any) any {
	data, err := sonic.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if err := sonicUseNumber.Unmarshal(data, &normalized); err != nil {
		return value
	}
	return expandJSONStrings(normalized)
}

func normalizeMapValue(value any) map[string]any {
	normalized := Normalize(value)
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

func (r *Recorder) appendEventLocked(kind string, value map[string]any) error {
	r.seq++
	eventValue, err := r.compactedMapValueLocked(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.eventsPath != "" {
		file, err := os.OpenFile(r.eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if err := writeSlogJSONEvent(file, r.seq, now, kind, eventValue); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	if r.eventWriter != nil {
		return writeConsoleTraceEvent(r.eventWriter, now, kind, eventValue)
	}
	return nil
}

func writeSlogJSONEvent(writer io.Writer, seq int, at time.Time, kind string, value map[string]any) error {
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{ReplaceAttr: dropSlogEnvelopeAttr})
	record := slog.NewRecord(at, slog.LevelInfo, "", 0)
	record.AddAttrs(
		slog.Int("seq", seq),
		slog.String("at", at.Format(time.RFC3339Nano)),
		slog.String("kind", kind),
		slog.Any("value", value),
	)
	return handler.Handle(context.Background(), record)
}

func writeConsoleTraceEvent(writer io.Writer, at time.Time, kind string, value map[string]any) error {
	return writeConsoleEvent(writer, at, kind, consoleEventValue(kind, value), consoleColorEnabled(writer))
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
	addConsoleValue(result, "instructions", value["instructions"])
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

func dropSlogEnvelopeAttr(_ []string, attr slog.Attr) slog.Attr {
	switch attr.Key {
	case slog.TimeKey, slog.LevelKey, slog.MessageKey:
		return slog.Attr{}
	default:
		return attr
	}
}

func (r *Recorder) applyLocked(kind string, value map[string]any) error {
	switch kind {
	case "session":
		maps.Copy(r.snapshot.Session, value)
	case "request":
		return r.applyRequestLocked(value)
	case "response":
		r.applyResponseLocked(value)
	case "tool-call":
		return r.applyToolCallLocked(value)
	case "tool-output":
		return r.applyToolOutputLocked(value)
	case "budget":
		r.applyBudgetLocked(value)
	case "final":
		r.snapshot.Final = maps.Clone(value)
		r.snapshot.Error = nil
	case "error":
		r.snapshot.Error = maps.Clone(value)
	default:
		// Keep the event in NDJSON even if the compact snapshot does not model it yet.
	}
	return nil
}

func (r *Recorder) applyRequestLocked(value map[string]any) error {
	input, ok := value["input"].([]any)
	if !ok {
		return fmt.Errorf("trace request missing input array: %#v", value["input"])
	}
	if err := r.appendRequestItemsLocked(input); err != nil {
		return err
	}

	if _, ok := r.snapshot.Static["model"]; !ok {
		if model, ok := value["model"]; ok {
			r.snapshot.Static["model"] = model
		}
	}
	if _, ok := r.snapshot.Static["tools"]; !ok {
		if tools, ok := value["tools"].([]any); ok && len(tools) > 0 {
			r.snapshot.Static["tools"] = tools
		}
	}

	r.snapshot.Steps = append(r.snapshot.Steps, step{
		Step: len(r.snapshot.Steps) + 1,
		Request: &stepRequest{
			Model:        value["model"],
			Instructions: value["instructions"],
			InputEnd:     len(r.snapshot.Items),
		},
	})
	r.currentStep = len(r.snapshot.Steps) - 1
	r.openToolPairByCall = map[string]int{}
	return nil
}

func (r *Recorder) appendRequestItemsLocked(input []any) error {
	existing := r.snapshot.Items
	if len(existing) > len(input) {
		return fmt.Errorf("trace request input shrank from %d to %d items", len(existing), len(input))
	}
	for idx := range existing {
		if !reflect.DeepEqual(stripItemIndex(existing[idx]), input[idx]) {
			return fmt.Errorf("trace request input diverged at item %d: have=%s want=%s", idx, mustJSON(stripItemIndex(existing[idx])), mustJSON(input[idx]))
		}
	}
	for _, item := range input[len(existing):] {
		if _, err := r.appendItemLocked(item); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) applyResponseLocked(value map[string]any) {
	current := r.currentStepPtr()
	if current == nil {
		return
	}
	response := &stepResponse{
		ID:         value["id"],
		FinishKind: value["finish_kind"],
		Text:       value["text"],
	}
	if toolCalls, ok := value["tool_calls"].([]any); ok && len(toolCalls) > 0 {
		response.ToolCalls = make([]int, 0, len(toolCalls))
	}
	current.Response = response
}

func (r *Recorder) applyToolCallLocked(value map[string]any) error {
	current := r.currentStepPtr()
	if current == nil {
		return errors.New("trace tool-call recorded before any request step")
	}

	item := maps.Clone(value)
	item["type"] = "function_call"
	itemIdx, err := r.appendItemLocked(item)
	if err != nil {
		return err
	}

	callID := stringField(item, "call_id")
	if callID == "" {
		callID = stringField(item, "id")
	}
	if callID != "" {
		r.openToolPairByCall[callID] = len(current.Tools)
	}
	current.Tools = append(current.Tools, stepTool{CallItem: itemIdx})
	if current.Response == nil {
		current.Response = &stepResponse{}
	}
	current.Response.ToolCalls = append(current.Response.ToolCalls, itemIdx)
	return nil
}

func (r *Recorder) applyToolOutputLocked(value map[string]any) error {
	current := r.currentStepPtr()
	if current == nil {
		return errors.New("trace tool-output recorded before any request step")
	}

	item := map[string]any{
		"type":    "function_call_output",
		"call_id": value["call_id"],
	}
	if content, ok := value["content"]; ok {
		item["output"] = content
	}
	itemIdx, err := r.appendItemLocked(item)
	if err != nil {
		return err
	}

	callID := stringField(item, "call_id")
	if pairIdx, ok := r.openToolPairByCall[callID]; ok && pairIdx < len(current.Tools) {
		current.Tools[pairIdx].OutputItem = itemIdx
		current.Tools[pairIdx].Trace = maps.Clone(value)
	} else {
		current.Tools = append(current.Tools, stepTool{
			OutputItem: itemIdx,
			Trace:      maps.Clone(value),
		})
	}
	return nil
}

func (r *Recorder) applyBudgetLocked(value map[string]any) {
	current := r.currentStepPtr()
	if current == nil {
		return
	}
	current.Budgets = append(current.Budgets, maps.Clone(value))
}

func (r *Recorder) appendItemLocked(value any) (int, error) {
	item, ok := value.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("trace item must be object, got %T", value)
	}
	copied := maps.Clone(item)
	copied["idx"] = len(r.snapshot.Items)
	r.snapshot.Items = append(r.snapshot.Items, copied)
	return copied["idx"].(int), nil
}

func (r *Recorder) currentStepPtr() *step {
	if r.currentStep < 0 || r.currentStep >= len(r.snapshot.Steps) {
		return nil
	}
	return &r.snapshot.Steps[r.currentStep]
}

func (r *Recorder) writeSnapshotLocked() error {
	if r.inMemory {
		return nil
	}

	snapshotValue, err := r.compactedSnapshotValueLocked()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	encoder := sonic.ConfigDefault.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshotValue); err != nil {
		return err
	}
	tmpPath := r.sessionPath + ".tmp"
	data := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, r.sessionPath)
}

func (r *Recorder) compactedSnapshotValueLocked() (any, error) {
	return r.compactedValueLocked(r.snapshot)
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
		if len(value) < largeStringArtifactThreshold {
			return value, nil
		}
		if r.inMemory {
			return inlineStringPreview(value), nil
		}
		return r.writeStringArtifactLocked(value)
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

func (r *Recorder) writeStringArtifactLocked(value string) (map[string]any, error) {
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:])
	relativePath := filepath.ToSlash(filepath.Join("artifacts", hash+".txt"))
	path := filepath.Join(r.dir, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return map[string]any{
		"artifact_ref": "artifact://sha256/" + hash,
		"path":         relativePath,
		"sha256":       hash,
		"bytes":        len(value),
		"preview":      stringPreview(value, artifactPreviewBytes),
		"truncated":    true,
	}, nil
}

func stringPreview(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	preview := value[:maxBytes]
	for !utf8.ValidString(preview) && len(preview) > 0 {
		preview = preview[:len(preview)-1]
	}
	return strings.TrimRight(preview, "\n") + "\n[artifact truncated]\n"
}

func stripItemIndex(item map[string]any) map[string]any {
	copied := maps.Clone(item)
	delete(copied, "idx")
	return copied
}

func stringField(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok {
		return ""
	}
	text, _ := raw.(string)
	return text
}

func mustJSON(value any) string {
	data, err := sonic.MarshalString(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return data
}
