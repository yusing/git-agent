package jsonx

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"testing"
)

func TestUseNumberPreservesNestedIntegers(t *testing.T) {
	t.Parallel()

	var value any
	if err := json.Unmarshal([]byte(`{"n":9007199254740993,"nested":[1]}`), &value, UseNumber); err != nil {
		t.Fatal(err)
	}
	root, _ := value.(map[string]any)
	if root["n"] != jsonv1.Number("9007199254740993") {
		t.Fatalf("nested object number = %#v", root["n"])
	}
	items, _ := root["nested"].([]any)
	if len(items) != 1 || items[0] != jsonv1.Number("1") {
		t.Fatalf("nested array number = %#v", items)
	}
}

func TestExtraJSONDetectsSecondValue(t *testing.T) {
	t.Parallel()

	var value map[string]any
	err := json.Unmarshal([]byte(`{} {}`), &value)
	if !ExtraJSON(err) {
		t.Fatalf("error = %v", err)
	}
}
