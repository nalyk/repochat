package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is the JSON type of a tool parameter.
type Kind int

const (
	String Kind = iota
	Integer
	Boolean
	StringList
	Object // any JSON value; Param.Raw is its JSON Schema, advertised as is
)

// Param describes one tool argument. The same description produces the JSON
// Schema advertised to clients and drives server-side validation, so the two
// can never drift apart.
type Param struct {
	Name     string
	Kind     Kind
	Desc     string
	Required bool
	Enum     []string // String only
	Min, Max int      // Integer only; applied (by clamping) when Max > Min
	MaxLen   int      // String: max length; StringList: max length per item
	MaxItems int      // StringList only

	// Object only: Raw is the JSON Schema of the value, advertised as is (a
	// "description" is added to a copy). MaxBytes caps the encoded value
	// (default defaultObjectBytes). The server checks presence and size; the
	// handler owns the structure.
	Raw      map[string]any
	MaxBytes int
}

// defaultObjectBytes is the size cap of an Object argument without MaxBytes.
const defaultObjectBytes = 1 << 20

// Schema is the ordered list of parameters a tool accepts.
type Schema []Param

// JSON renders the schema as a JSON Schema (2020-12) object. "required" is
// omitted when empty and is never null; unknown properties are rejected.
func (s Schema) JSON() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range s {
		prop := map[string]any{"description": p.Desc}
		switch p.Kind {
		case String:
			prop["type"] = "string"
			if len(p.Enum) > 0 {
				prop["enum"] = append([]string(nil), p.Enum...)
			}
			if p.MaxLen > 0 {
				prop["maxLength"] = p.MaxLen
			}
			if p.Required {
				prop["minLength"] = 1
			}
		case Integer:
			prop["type"] = "integer"
			if p.Max > p.Min {
				prop["minimum"] = p.Min
				prop["maximum"] = p.Max
			}
		case Boolean:
			prop["type"] = "boolean"
		case Object:
			for k, v := range p.Raw {
				prop[k] = v
			}
			prop["description"] = p.Desc
		case StringList:
			prop["type"] = "array"
			item := map[string]any{"type": "string"}
			if p.MaxLen > 0 {
				item["maxLength"] = p.MaxLen
			}
			prop["items"] = item
			if p.MaxItems > 0 {
				prop["maxItems"] = p.MaxItems
			}
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// Signature is a one-line human/model readable summary used in error messages.
func (s Schema) Signature() string {
	if len(s) == 0 {
		return "(no arguments)"
	}
	parts := make([]string, 0, len(s))
	for _, p := range s {
		t := map[Kind]string{String: "string", Integer: "integer", Boolean: "boolean", StringList: "string[]", Object: "object"}[p.Kind]
		name := p.Name
		if !p.Required {
			name += "?"
		}
		parts = append(parts, name+": "+t)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Args holds validated, normalised arguments: string, int, bool, []string or json.RawMessage.
type Args map[string]any

func (a Args) Has(k string) bool { _, ok := a[k]; return ok }

func (a Args) String(k string) string {
	s, _ := a[k].(string)
	return s
}

func (a Args) Int(k string, def int) int {
	if n, ok := a[k].(int); ok {
		return n
	}
	return def
}

func (a Args) Bool(k string) bool {
	b, _ := a[k].(bool)
	return b
}

// Raw returns an Object argument as validated JSON; nil when absent.
func (a Args) Raw(k string) json.RawMessage {
	r, _ := a[k].(json.RawMessage)
	return r
}

func (a Args) Strings(k string) []string {
	l, _ := a[k].([]string)
	return l
}

// Validate checks raw JSON-decoded arguments against the schema. It is lenient
// where leniency is harmless (numeric strings for integers, a bare string for
// a one-element list, out-of-range integers are clamped) and strict where a
// silent guess would mislead the model (unknown names, wrong types, enums).
func (s Schema) Validate(raw map[string]any) (Args, error) {
	known := map[string]bool{}
	for _, p := range s {
		known[p.Name] = true
	}
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown argument(s) %s; expected %s", strings.Join(unknown, ", "), s.Signature())
	}
	out := Args{}
	for _, p := range s {
		v, ok := raw[p.Name]
		if !ok || v == nil {
			if p.Required {
				return nil, fmt.Errorf("missing required argument %q; expected %s", p.Name, s.Signature())
			}
			continue
		}
		switch p.Kind {
		case String:
			str, err := asString(v)
			if err != nil {
				return nil, fmt.Errorf("argument %q must be a string", p.Name)
			}
			if err := checkString(p, str); err != nil {
				return nil, err
			}
			// Only the empty string is refused, as advertised (minLength 1):
			// whitespace is a value, and a search for a tab is a search.
			if p.Required && str == "" {
				return nil, fmt.Errorf("argument %q must not be empty", p.Name)
			}
			out[p.Name] = str
		case Integer:
			n, err := asInt(v)
			if err != nil {
				return nil, fmt.Errorf("argument %q must be an integer", p.Name)
			}
			if p.Max > p.Min {
				if n < p.Min {
					n = p.Min
				}
				if n > p.Max {
					n = p.Max
				}
			}
			out[p.Name] = n
		case Boolean:
			switch b := v.(type) {
			case bool:
				out[p.Name] = b
			case string:
				switch strings.ToLower(strings.TrimSpace(b)) {
				case "true":
					out[p.Name] = true
				case "false":
					out[p.Name] = false
				default:
					return nil, fmt.Errorf("argument %q must be a boolean", p.Name)
				}
			default:
				return nil, fmt.Errorf("argument %q must be a boolean", p.Name)
			}
		case Object:
			enc, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("argument %q is not valid JSON", p.Name)
			}
			max := p.MaxBytes
			if max <= 0 {
				max = defaultObjectBytes
			}
			if len(enc) > max {
				return nil, fmt.Errorf("%s: too large (%d bytes, max %d)", p.Name, len(enc), max)
			}
			out[p.Name] = json.RawMessage(enc)
		case StringList:
			var list []string
			switch l := v.(type) {
			case string:
				list = []string{l}
			case []any:
				for _, item := range l {
					str, ok := item.(string)
					if !ok {
						return nil, fmt.Errorf("argument %q must be an array of strings", p.Name)
					}
					list = append(list, str)
				}
			default:
				return nil, fmt.Errorf("argument %q must be an array of strings", p.Name)
			}
			if p.MaxItems > 0 && len(list) > p.MaxItems {
				return nil, fmt.Errorf("argument %q accepts at most %d items (got %d)", p.Name, p.MaxItems, len(list))
			}
			for _, item := range list {
				if err := checkString(p, item); err != nil {
					return nil, err
				}
			}
			out[p.Name] = list
		}
	}
	return out, nil
}

func checkString(p Param, s string) error {
	// maxLength counts characters (code points), as JSON Schema defines it;
	// counting bytes would refuse non-ASCII text the schema allows.
	if p.MaxLen > 0 && len(s) > p.MaxLen {
		if n := utf8.RuneCountInString(s); n > p.MaxLen {
			return fmt.Errorf("argument %q is too long (%d characters, max %d)", p.Name, n, p.MaxLen)
		}
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("argument %q must not contain NUL bytes", p.Name)
	}
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			if e == s {
				return nil
			}
		}
		return fmt.Errorf("argument %q must be one of: %s", p.Name, strings.Join(p.Enum, ", "))
	}
	return nil
}

func asString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		// Models sometimes send an all-digit commit prefix as a number.
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatFloat(t, 'f', -1, 64), nil
		}
	}
	return "", fmt.Errorf("not a string")
}

func asInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case float64:
		if t != math.Trunc(t) || math.Abs(t) > 1e12 {
			return 0, fmt.Errorf("not an integer")
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, fmt.Errorf("not an integer")
}
