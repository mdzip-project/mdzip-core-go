package mdzip

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// nfc returns s in Unicode Normalization Form C, the form paths are compared
// in (Section 5.4.1).
func nfc(s string) string { return norm.NFC.String(s) }

// caseFold applies Unicode default full case folding. It is used only to
// warn about case-only differences, never to match paths.
var folder = cases.Fold()

func caseFold(s string) string { return folder.String(s) }

// asciiLower lowercases A–Z only. Support file names and Markdown extensions
// are compared this way, never with locale rules (Section 5.4.1).
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// IsMarkdown reports whether name has a .md or .markdown extension, compared
// ASCII case-insensitively (Section 7.3).
func IsMarkdown(name string) bool {
	l := asciiLower(name)
	return strings.HasSuffix(l, ".md") || strings.HasSuffix(l, ".markdown")
}

var supportStems = map[string]bool{
	"readme": true, "agents": true, "license": true, "changelog": true, "contributing": true,
}

// isSupportFile reports whether a root-level path is a support file for rule
// 3 of entry point discovery (Section 5.5).
func isSupportFile(path string) bool {
	if strings.Contains(path, "/") {
		return false
	}
	l := asciiLower(path)
	switch {
	case strings.HasSuffix(l, ".markdown"):
		l = strings.TrimSuffix(l, ".markdown")
	case strings.HasSuffix(l, ".md"):
		l = strings.TrimSuffix(l, ".md")
	default:
		return false
	}
	return supportStems[l]
}

// ValidatePath reports whether name is a valid stored entry path under
// Section 5.4. A name ending in "/" is a directory entry and is checked
// without that slash. The error is an *Error with Code ErrPathInvalid and,
// where one applies, a Detail such as ErrPathTraversal.
//
// ValidatePath checks a single name. Whole-archive rules (NFC duplicates, a
// file that is also a directory) are checked when an archive is opened.
func ValidatePath(name string) error {
	bad := func(detail Code, msg string) error {
		return &Error{Code: ErrPathInvalid, Detail: detail, Path: name, Message: msg}
	}
	if name == "" {
		return bad(ErrPathEmpty, "path is empty")
	}
	if !utf8.ValidString(name) {
		return bad("", "path is not valid UTF-8")
	}
	if strings.HasPrefix(name, "/") {
		return bad(ErrPathLeadingSlash, "path starts with \"/\"")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return bad(ErrPathControlChar, "path contains a control character")
		}
		if strings.ContainsRune(`\:*?"<>|`, r) {
			return bad(ErrPathReservedChar, "path contains the reserved character "+string(r))
		}
	}
	body := strings.TrimSuffix(name, "/")
	if body == "" {
		return bad(ErrPathEmpty, "path is empty")
	}
	segs := strings.Split(body, "/")
	for _, s := range segs {
		if s == ".." {
			return bad(ErrPathTraversal, "path contains a \"..\" segment")
		}
	}
	for _, s := range segs {
		if s == "" || s == "." {
			return bad("", "path contains an empty or \".\" segment")
		}
	}
	return nil
}

// cleanEntryPoint checks a manifest entryPoint value against the entry point
// path rules of Section 5.5 and returns it without a single leading "./".
func cleanEntryPoint(v string) (string, error) {
	invalid := func(msg string) error {
		return &Error{Code: ErrEntryPointInvalid, Path: v, Message: msg}
	}
	if v == "" {
		return "", invalid("entryPoint is empty")
	}
	p := strings.TrimPrefix(v, "./")
	if strings.HasSuffix(p, "/") {
		return "", invalid("entryPoint names a directory")
	}
	if err := ValidatePath(p); err != nil {
		return "", &Error{Code: ErrEntryPointInvalid, Path: v, Message: "entryPoint is not a valid archive path", Err: err}
	}
	if !IsMarkdown(p) {
		return "", invalid("entryPoint is not a .md or .markdown file")
	}
	return p, nil
}

// cleanCover checks a manifest cover value (Section 6.2) and returns it
// without a single leading "./".
func cleanCover(v string) (string, bool) {
	p := strings.TrimPrefix(v, "./")
	if p == "" || strings.HasSuffix(p, "/") || ValidatePath(p) != nil {
		return "", false
	}
	return p, true
}
