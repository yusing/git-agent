package jsonx

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"strings"
)

// UseNumber unmarshals JSON numbers into encoding/json.Number when the
// destination is any, including nested object and array values.
var UseNumber = json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, v *any) error {
	if dec.PeekKind() != '0' {
		return errors.ErrUnsupported
	}
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	*v = jsonv1.Number(raw)
	return nil
}))

// ExtraJSON reports whether err is from leftover input after one JSON value.
func ExtraJSON(err error) bool {
	return err != nil && strings.Contains(err.Error(), "after top-level value")
}
