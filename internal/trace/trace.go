package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

type Recorder struct {
	dir                string
	eventsPath         string
	sessionPath        string
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

type eventRecord struct {
	Seq   int            `json:"seq"`
	At    string         `json:"at"`
	Kind  string         `json:"kind"`
	Value map[string]any `json:"value,omitempty"`
}

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
	if err := r.appendEventLocked(kind, value); err != nil {
		return err
	}
	if err := r.applyLocked(kind, value); err != nil {
		return err
	}
	return r.writeSnapshotLocked()
}

func Normalize(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
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
	trimmed := bytes.TrimSpace([]byte(value))
	if len(trimmed) == 0 {
		return value
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return value
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var expanded any
	if err := decoder.Decode(&expanded); err != nil {
		return value
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value
	}
	return expandJSONStrings(expanded)
}

func (r *Recorder) appendEventLocked(kind string, value map[string]any) error {
	r.seq++
	file, err := os.OpenFile(r.eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	record := eventRecord{
		Seq:   r.seq,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
		Kind:  kind,
		Value: value,
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(record)
}

func (r *Recorder) applyLocked(kind string, value map[string]any) error {
	switch kind {
	case "session":
		for key, item := range value {
			r.snapshot.Session[key] = item
		}
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
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r.snapshot); err != nil {
		return err
	}
	tmpPath := r.sessionPath + ".tmp"
	data := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, r.sessionPath)
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
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return string(data)
}
