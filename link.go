package mdzip

import (
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

// LinkKind says what a link destination resolved to.
type LinkKind int

const (
	// LinkUnresolved: the link has no target; see [Link.Reason]. Show it as
	// broken. Never substitute another target for it.
	LinkUnresolved LinkKind = iota
	// LinkInternal: an entry in the same archive, in [Link.Entry].
	LinkInternal
	// LinkURL: a URL with a scheme, such as https: or mailto:. It is not a
	// path and isn't resolved (Section 9.2).
	LinkURL
)

// Reason says why a link is unresolved. Reasons are informative; the kind is
// what conformance depends on.
type Reason string

const (
	// ReasonNotFound: the path is well formed but names no file, including a
	// name that differs only in case, a directory or a symbolic link entry.
	ReasonNotFound Reason = "not-found"
	// ReasonInvalid: the destination isn't a valid relative link, for
	// example "/x.md", "a\b.md", "%ZZ" or an encoded "/".
	ReasonInvalid Reason = "invalid"
	// ReasonOutside: the link climbs above the archive root. This library
	// doesn't resolve links outside or into archives (Sections 9.4 and 9.5,
	// both optional), so they stay unresolved; a host application may
	// resolve them itself under the rules of Section 9.6.
	ReasonOutside Reason = "unsupported"
)

// Link is the result of [Archive.Resolve].
type Link struct {
	Kind LinkKind
	// Entry is the stored path of the target, for LinkInternal.
	Entry string
	// Fragment is the text after the first "#", or "". It never takes part
	// in resolving the path.
	Fragment string
	// Reason is why a LinkUnresolved link has no target.
	Reason Reason
}

// Resolve resolves a standard Markdown link destination (from a link,
// image, reference definition or HTML href/src) written in the document at
// from, using the algorithm of Section 9.3.
//
// Paths are resolved against from's directory, percent-decoded once per
// segment and matched after NFC normalization, case-sensitively. A ".." that
// climbs above the archive root is never clamped: the link is unresolved
// with ReasonOutside. A destination of only a fragment or query refers to
// from itself.
//
// Resolve fails with ErrNotFound only when from isn't a file in the archive.
func (a *Archive) Resolve(from, dest string) (Link, error) {
	src := a.file(from)
	if src == nil {
		return Link{}, newError(ErrNotFound, from, "the referencing document is not a file in the archive")
	}
	if hasScheme(dest) {
		return Link{Kind: LinkURL}, nil
	}
	ref, frag, _ := strings.Cut(dest, "#")
	if i := strings.IndexByte(ref, '?'); i >= 0 {
		ref = ref[:i]
	}
	unresolved := func(r Reason) (Link, error) {
		return Link{Kind: LinkUnresolved, Fragment: frag, Reason: r}, nil
	}
	if ref == "" {
		return Link{Kind: LinkInternal, Entry: src.name, Fragment: frag}, nil
	}
	if strings.HasPrefix(ref, "/") || strings.Contains(ref, `\`) {
		return unresolved(ReasonInvalid)
	}
	raw := strings.Split(ref, "/")
	segs := make([]string, len(raw))
	for i, r := range raw {
		d, err := url.PathUnescape(r)
		if err != nil || !utf8.ValidString(d) || strings.ContainsAny(d, `/\`) || hasControl(d) {
			return unresolved(ReasonInvalid)
		}
		segs[i] = d
	}
	var dir []string
	if d := path.Dir(src.key); d != "." {
		dir = strings.Split(d, "/")
	}
	for _, s := range segs {
		switch s {
		case "", ".":
		case "..":
			if len(dir) == 0 {
				return unresolved(ReasonOutside)
			}
			dir = dir[:len(dir)-1]
		default:
			dir = append(dir, s)
		}
	}
	if strings.HasSuffix(ref, "/") || len(dir) == 0 {
		return unresolved(ReasonNotFound) // a directory, not a file
	}
	if e := a.file(strings.Join(dir, "/")); e != nil {
		return Link{Kind: LinkInternal, Entry: e.name, Fragment: frag}, nil
	}
	return unresolved(ReasonNotFound)
}

// hasScheme reports whether dest starts with a URL scheme (RFC 3986): a
// letter, then letters, digits, "+", "-" or ".", then ":", before any "/",
// "?" or "#".
func hasScheme(dest string) bool {
	for i := 0; i < len(dest); i++ {
		c := dest[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		case c == ':':
			return i > 0
		default:
			return false
		}
	}
	return false
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
