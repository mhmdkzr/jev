package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrInvalidValue is returned when an uninitialized [Value] is marshaled. A
// Value must be built with [Text], [JSON], or [MustJSON].
var ErrInvalidValue = errors.New("jev: uninitialized Value; use jev.Text or jev.JSON")

// Value is a validated, JSON-serializable input for state or instructions. It
// is created only by [Text], [JSON], or [MustJSON]; the zero Value is invalid
// and [Request.Send] reports it rather than sending null. A Value snapshots
// its input at construction, so later mutations of the original data cannot
// change a request or race with it.
type Value struct {
	encoded []byte
	text    string
	isText  bool
	isNull  bool
	valid   bool
}

// Null returns a [Value] representing JSON null. It is accepted where the API
// allows an undescribed entry (Score levels, Choice options, Noul criteria) and
// rejected for state and instructions.
func Null() Value {
	return Value{encoded: []byte("null"), isNull: true, valid: true}
}

// IsNull reports whether the value is JSON null.
func (v Value) IsNull() bool { return v.isNull }

// Text builds a [Value] from a plain string.
func Text(s string) Value {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic("jev: Text: " + err.Error())
	}
	return Value{encoded: encoded, text: s, isText: true, valid: true}
}

// JSON builds a [Value] from any JSON-marshalable Go value. It encodes
// immediately, so an unmarshalable or nil value is reported here rather than
// at [Request.Send].
func JSON(v any) (Value, error) {
	if isNilValue(v) {
		return Value{}, errors.New("jev: JSON value must not be nil")
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return Value{}, fmt.Errorf("jev: encode value: %w", err)
	}
	if bytes.Equal(encoded, []byte("null")) {
		return Value{}, errors.New("jev: value encoded to null")
	}
	value := Value{encoded: encoded, valid: true}
	// Remember string content so blank strings can be rejected uniformly,
	// whether they came from Text or JSON.
	if len(encoded) > 0 && encoded[0] == '"' {
		var text string
		if err := json.Unmarshal(encoded, &text); err == nil {
			value.text = text
			value.isText = true
		}
	}
	return value, nil
}

// MustJSON is like [JSON] but panics on error. Use it only with values known
// to be JSON-marshalable and non-nil.
func MustJSON(v any) Value {
	value, err := JSON(v)
	if err != nil {
		panic(err)
	}
	return value
}

// MarshalJSON implements [json.Marshaler]. It fails loudly on an
// uninitialized Value instead of emitting null.
func (v Value) MarshalJSON() ([]byte, error) {
	if !v.valid {
		return nil, ErrInvalidValue
	}
	return v.encoded, nil
}

// UnmarshalJSON implements [json.Unmarshaler], so a Value can carry a
// structured description back from the API (for example a Score legend entry).
// A null value is rejected.
func (v *Value) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errors.New("jev: empty JSON value")
	}
	if bytes.Equal(data, []byte("null")) {
		*v = Null()
		return nil
	}
	encoded := make([]byte, len(data))
	copy(encoded, data)
	*v = Value{encoded: encoded, valid: true}
	if encoded[0] == '"' {
		var text string
		if err := json.Unmarshal(encoded, &text); err == nil {
			v.text = text
			v.isText = true
		}
	}
	return nil
}

// Bytes returns a copy of the raw JSON encoding of the value, or nil if the
// value is uninitialized.
func (v Value) Bytes() []byte {
	if !v.valid {
		return nil
	}
	out := make([]byte, len(v.encoded))
	copy(out, v.encoded)
	return out
}

// Text returns the string content of the value and whether it is a JSON
// string rather than a structured object or array.
func (v Value) Text() (string, bool) {
	return v.text, v.isText
}

// Decode unmarshals the value into out.
func (v Value) Decode(out any) error {
	if !v.valid {
		return ErrInvalidValue
	}
	return json.Unmarshal(v.encoded, out)
}

// String returns the text of a string value, or its raw JSON otherwise.
func (v Value) String() string {
	if !v.valid {
		return ""
	}
	if v.isText {
		return v.text
	}
	return string(v.encoded)
}

// StateValue is the constraint for state: a bare string or a validated
// [Value].
type StateValue interface{ ~string | Value }

// EntryValue is the constraint for a JSON entry: instructions, a Choice option
// description, a Score level, or a Noul true/false description. Each may be a
// bare string or a structured [Value].
type EntryValue interface{ ~string | Value }

// normalize converts a constrained input into a Value. It handles named
// string types as well as the exact string type.
func normalize[I interface{ ~string | Value }](v I) Value {
	if value, ok := any(v).(Value); ok {
		return value
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
		return Text(rv.String())
	}
	return Value{}
}

// validateValue reports whether v is usable, giving it a friendly name.
func validateValue(v Value, name string) error {
	if !v.valid {
		return fmt.Errorf("%s is required", name)
	}
	if v.isNull {
		return fmt.Errorf("%s must not be null", name)
	}
	if v.isText && strings.TrimSpace(v.text) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return nil
}

// isNilValue reports whether v is nil or a typed nil pointer, map, slice,
// channel, function, or interface, all of which json.Marshal would silently
// encode as null.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
