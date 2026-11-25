package mdzip

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SpecVersion is the version of the MDZip specification this library
// implements and writes into new manifests.
const SpecVersion = "1.2.0"

// supportedMajor is the highest spec major version this library reads.
const supportedMajor = 1

// Mode is an archive's interpretation mode (Section 5.7).
type Mode string

const (
	// ModeDocument: one logical publication (the default).
	ModeDocument Mode = "document"
	// ModeProject: a collection of independent documents.
	ModeProject Mode = "project"
)

// Manifest is a parsed manifest.json. It keeps every field, including ones
// this library doesn't know, so rewriting it preserves them (Section 6.3).
// The typed accessors return "" for a field that is absent or invalid.
type Manifest struct {
	obj      *object
	warnings []Finding
	dirty    bool // changed since it was read
	isNew    bool // made by this library, not read from an archive
}

// ParseManifest parses and validates manifest.json (Section 6.4).
//
// It fails with ErrManifestInvalid when the data isn't UTF-8 JSON, the
// top-level value isn't an object, or spec or spec.version is wrong; with
// ErrVersionUnsupported for a spec.version major version above 1; with
// ErrModeUnsupported for an unknown mode string; and with
// ErrEntryPointInvalid for an entryPoint that isn't a valid entry point path.
// Wrong descriptive fields are ignored and reported in [Manifest.Warnings],
// as are duplicate member names (the last one is used).
func ParseManifest(data []byte) (*Manifest, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, newError(ErrManifestInvalid, "manifest.json", "not valid UTF-8")
	}
	v, dups, err := parseJSON(data)
	if err != nil {
		return nil, &Error{Code: ErrManifestInvalid, Path: "manifest.json", Message: "not valid JSON", Err: err}
	}
	obj, ok := v.(*object)
	if !ok {
		return nil, newError(ErrManifestInvalid, "manifest.json", "the top-level value is not an object")
	}
	m := &Manifest{obj: obj}
	for _, p := range dups {
		m.warnings = append(m.warnings, Finding{Severity: SeverityWarning, Code: WarnDuplicateMember, Path: p,
			Message: "manifest.json repeats this member name; the last one is used"})
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func newManifest() *Manifest { return &Manifest{obj: newObject(), dirty: true, isNew: true} }

var semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

var rfc3339RE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`)

func isRFC3339(s string) bool {
	if !rfc3339RE.MatchString(s) {
		return false
	}
	u := strings.ToUpper(s)
	_, err := time.Parse(time.RFC3339Nano, u)
	if err == nil {
		return true
	}
	// time.Parse rejects a leap second; RFC 3339 allows second 60.
	if len(u) > 19 && u[17:19] == "60" {
		_, err = time.Parse(time.RFC3339Nano, u[:17]+"59"+u[19:])
		return err == nil
	}
	return false
}

func (m *Manifest) ignore(field, msg string) {
	m.warnings = append(m.warnings, Finding{Severity: SeverityWarning, Code: WarnFieldIgnored, Path: field,
		Message: msg})
}

// validate applies Section 6.4: critical fields fail, descriptive ones are
// ignored with a warning, unknown ones are never checked.
func (m *Manifest) validate() error {
	o := m.obj
	// Critical: spec and spec.version.
	if v, ok := o.get("spec"); ok {
		spec, isObj := v.(*object)
		if !isObj {
			return newError(ErrManifestInvalid, "spec", "spec is not an object")
		}
		if sv, ok := spec.get("version"); ok {
			s, isStr := sv.(string)
			if !isStr || !semverRE.MatchString(s) {
				return newError(ErrManifestInvalid, "spec.version", "spec.version is not a Semantic Versioning string")
			}
			major, _ := strconv.Atoi(s[:strings.IndexByte(s, '.')])
			if major > supportedMajor {
				return newError(ErrVersionUnsupported, "spec.version",
					"spec.version "+s+" needs a newer MDZip reader (this one reads major version 1)")
			}
			if major < supportedMajor {
				m.warnings = append(m.warnings, Finding{Severity: SeverityWarning, Code: WarnVersionOlder,
					Path: "spec.version", Message: "spec.version " + s + " is older than major version 1"})
			}
		}
		if n, ok := spec.get("name"); ok {
			if _, isStr := n.(string); !isStr {
				m.ignore("spec.name", "spec.name is not a string")
			}
		}
	}
	// Critical: mode.
	if v, ok := o.get("mode"); ok {
		s, isStr := v.(string)
		if !isStr {
			return newError(ErrManifestInvalid, "mode", "mode is not a string")
		}
		if Mode(s) != ModeDocument && Mode(s) != ModeProject {
			return newError(ErrModeUnsupported, "mode", "unrecognized mode "+strconv.Quote(s))
		}
	}
	// Critical: entryPoint.
	if v, ok := o.get("entryPoint"); ok {
		s, isStr := v.(string)
		if !isStr {
			return newError(ErrEntryPointInvalid, "entryPoint", "entryPoint is not a string")
		}
		if _, err := cleanEntryPoint(s); err != nil {
			return err
		}
	}
	// Descriptive fields.
	if v, ok := o.get("title"); ok {
		if s, isStr := v.(string); !isStr || s == "" {
			m.ignore("title", "title is not a non-empty string")
		}
	}
	for _, f := range []string{"language", "description", "version", "license"} {
		if v, ok := o.get(f); ok {
			if _, isStr := v.(string); !isStr {
				m.ignore(f, f+" is not a string")
			}
		}
	}
	if v, ok := o.get("producer"); ok {
		if p, isObj := v.(*object); !isObj {
			m.ignore("producer", "producer is not an object")
		} else {
			for _, part := range []string{"application", "core"} {
				m.checkIdentity(p, part, "producer."+part, "name", "version", "url")
			}
		}
	}
	m.checkIdentity(o, "author", "author", "name", "email", "url")
	for _, f := range []string{"created", "modified"} {
		m.checkTimestamp(f)
	}
	if v, ok := o.get("keywords"); ok {
		good := false
		if a, isArr := v.([]any); isArr {
			good = true
			for _, x := range a {
				if _, isStr := x.(string); !isStr {
					good = false
				}
			}
		}
		if !good {
			m.ignore("keywords", "keywords is not an array of strings")
		}
	}
	if v, ok := o.get("cover"); ok {
		s, isStr := v.(string)
		if !isStr {
			m.ignore("cover", "cover is not a string")
		} else if _, valid := cleanCover(s); !valid {
			m.ignore("cover", "cover is not a valid archive path")
		}
	}
	return nil
}

// checkIdentity checks an optional object whose listed members are strings,
// such as author or producer.application.
func (m *Manifest) checkIdentity(parent *object, key, field string, members ...string) {
	v, ok := parent.get(key)
	if !ok {
		return
	}
	obj, isObj := v.(*object)
	if !isObj {
		m.ignore(field, field+" is not an object")
		return
	}
	for _, mem := range members {
		if x, ok := obj.get(mem); ok {
			if _, isStr := x.(string); !isStr {
				m.ignore(field+"."+mem, field+"."+mem+" is not a string")
			}
		}
	}
}

func (m *Manifest) checkTimestamp(f string) {
	v, ok := m.obj.get(f)
	if !ok {
		return
	}
	switch t := v.(type) {
	case string:
		if !isRFC3339(t) {
			m.ignore(f, f+" is not an RFC 3339 date-time")
		}
	case *object:
		w, ok := t.get("when")
		if s, isStr := w.(string); !ok || !isStr || !isRFC3339(s) {
			m.ignore(f, f+".when is missing or not an RFC 3339 date-time")
			return
		}
		m.checkIdentity(t, "by", f+".by", "name", "email", "url")
	default:
		m.ignore(f, f+" is neither an object nor a date-time string")
	}
}

// Warnings returns the problems found that don't stop the manifest being
// used: ignored descriptive fields and duplicate member names.
func (m *Manifest) Warnings() []Finding { return append([]Finding(nil), m.warnings...) }

func (m *Manifest) str(key string) string {
	v, _ := m.obj.get(key)
	s, _ := v.(string)
	return s
}

// Title returns the title, or "" if it is absent or invalid.
func (m *Manifest) Title() string { return m.str("title") }

// SetTitle sets the title. An empty title removes the field, since the
// specification doesn't allow an empty one.
func (m *Manifest) SetTitle(title string) {
	if title == "" {
		m.Delete("title")
		return
	}
	m.obj.set("title", title)
	m.dirty = true
}

// Mode returns the declared mode, or ModeDocument when there is none.
func (m *Manifest) Mode() Mode {
	if s := m.str("mode"); s != "" {
		return Mode(s)
	}
	return ModeDocument
}

// SetMode sets the mode. It accepts only ModeDocument and ModeProject.
func (m *Manifest) SetMode(mode Mode) error {
	if mode != ModeDocument && mode != ModeProject {
		return newError(ErrModeUnsupported, "mode", "unrecognized mode "+strconv.Quote(string(mode)))
	}
	m.obj.set("mode", string(mode))
	m.dirty = true
	return nil
}

// EntryPoint returns the declared entryPoint with any leading "./" removed,
// or "" when there is none. [Archive.EntryPoint] resolves it.
func (m *Manifest) EntryPoint() string {
	p, err := cleanEntryPoint(m.str("entryPoint"))
	if err != nil {
		return ""
	}
	return p
}

// SpecVersion returns spec.version, or "" when the manifest has none.
func (m *Manifest) SpecVersion() string {
	if spec := m.obj.child("spec", false); spec != nil {
		v, _ := spec.get("version")
		s, _ := v.(string)
		return s
	}
	return ""
}

// Language returns the BCP 47 language tag, or "".
func (m *Manifest) Language() string { return m.str("language") }

// Description returns the description, or "".
func (m *Manifest) Description() string { return m.str("description") }

// Cover returns the cover image path with any leading "./" removed, or ""
// when it is absent or invalid. The file may still be missing; opening an
// archive warns about that.
func (m *Manifest) Cover() string {
	p, ok := cleanCover(m.str("cover"))
	if !ok {
		return ""
	}
	return p
}

// SetApplication records the application writing the archive in
// producer.application, keeping any other fields there (Section 14.1).
func (m *Manifest) SetApplication(name, version, url string) {
	app := m.obj.child("producer", true).child("application", true)
	for _, kv := range [][2]string{{"name", name}, {"version", version}, {"url", url}} {
		if kv[1] != "" {
			app.set(kv[0], kv[1])
		}
	}
	m.dirty = true
}

// Lookup returns the value at a JSON Pointer (RFC 6901), such as "/title" or
// "/producer/application/name", as plain Go values: map[string]any, []any,
// string, json.Number, bool or nil. It reports false when nothing is there.
func (m *Manifest) Lookup(pointer string) (any, bool) {
	v, ok := lookupPointer(m.obj, pointer)
	if !ok {
		return nil, false
	}
	return toPlain(v), true
}

// Set sets a top-level field to any value encoding/json can marshal. Use it
// for fields without a typed setter. Setting a field the specification
// treats as critical checks it again: an invalid mode or entryPoint fails.
func (m *Manifest) Set(name string, value any) error {
	v, err := fromGo(value)
	if err != nil {
		return &Error{Code: ErrManifestInvalid, Path: name, Message: "value can't be encoded as JSON", Err: err}
	}
	old, had := m.obj.get(name)
	m.obj.set(name, v)
	saved := m.warnings
	m.warnings = nil
	if err := m.validate(); err != nil {
		if had {
			m.obj.set(name, old)
		} else {
			m.obj.del(name)
		}
		m.warnings = saved
		return err
	}
	m.dirty = true
	return nil
}

// Delete removes a top-level field.
func (m *Manifest) Delete(name string) {
	if _, ok := m.obj.get(name); ok {
		m.obj.del(name)
		m.dirty = true
	}
}

// MarshalJSON returns the manifest as written to manifest.json: standard
// JSON, two-space indentation, LF line endings and no duplicate members.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	writeJSON(&b, m.obj, "")
	return b.Bytes(), nil
}

func (m *Manifest) bytes() []byte {
	b, _ := m.MarshalJSON()
	return append(b, '\n')
}

var _ json.Marshaler = (*Manifest)(nil)
