package mdzip

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// object is a JSON object that keeps its members in order, so a rewritten
// manifest keeps unknown fields (Section 6.3) and a familiar layout.
// Values are nil, bool, json.Number, string, []any or *object.
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// child returns the object at key k, creating it (or replacing a non-object
// value) when create is true.
func (o *object) child(k string, create bool) *object {
	if v, ok := o.vals[k].(*object); ok {
		return v
	}
	if !create {
		return nil
	}
	c := newObject()
	o.set(k, c)
	return c
}

// parseJSON parses one JSON value. It returns JSON Pointers to any duplicate
// object members it met; the last occurrence wins, as Section 6.4 requires.
func parseJSON(data []byte) (any, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var dups []string
	v, err := parseValue(dec, "", &dups)
	if err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("unexpected data after the JSON value")
	}
	return v, dups, nil
}

func parseValue(dec *json.Decoder, ptr string, dups *[]string) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				childPtr := ptr + "/" + escapePointer(k)
				v, err := parseValue(dec, childPtr, dups)
				if err != nil {
					return nil, err
				}
				if _, seen := o.vals[k]; seen {
					*dups = append(*dups, childPtr)
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return o, nil
		case '[':
			a := []any{}
			for i := 0; dec.More(); i++ {
				v, err := parseValue(dec, ptr+"/"+strconv.Itoa(i), dups)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return a, nil
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return t, nil // nil, bool, json.Number, string
	}
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// lookupPointer resolves an RFC 6901 JSON Pointer against v.
func lookupPointer(v any, ptr string) (any, bool) {
	if ptr == "" {
		return v, true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	for _, raw := range strings.Split(ptr[1:], "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch c := v.(type) {
		case *object:
			n, ok := c.vals[tok]
			if !ok {
				return nil, false
			}
			v = n
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) || (len(tok) > 1 && tok[0] == '0') {
				return nil, false
			}
			v = c[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// toPlain converts the ordered model to plain Go values: map[string]any,
// []any, string, bool, json.Number and nil.
func toPlain(v any) any {
	switch c := v.(type) {
	case *object:
		m := make(map[string]any, len(c.keys))
		for _, k := range c.keys {
			m[k] = toPlain(c.vals[k])
		}
		return m
	case []any:
		a := make([]any, len(c))
		for i, x := range c {
			a[i] = toPlain(x)
		}
		return a
	}
	return v
}

// fromGo converts any value encoding/json can marshal into the ordered model.
func fromGo(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, _, err := parseJSON(b)
	return out, err
}

// writeJSON serializes v with two-space indentation and LF line endings,
// without escaping <, > and & (they are valid JSON as they are).
func writeJSON(b *bytes.Buffer, v any, indent string) {
	switch c := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(c))
	case json.Number:
		b.WriteString(string(c))
	case string:
		writeString(b, c)
	case []any:
		if len(c) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, x := range c {
			b.WriteString(indent + "  ")
			writeJSON(b, x, indent+"  ")
			if i < len(c)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	case *object:
		if len(c.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range c.keys {
			b.WriteString(indent + "  ")
			writeString(b, k)
			b.WriteString(": ")
			writeJSON(b, c.vals[k], indent+"  ")
			if i < len(c.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	}
}

func writeString(b *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	b.Write(bytes.TrimSuffix(tmp.Bytes(), []byte("\n")))
}
