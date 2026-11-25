package mdzip

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits are the resource limits applied while reading (Section 13.1).
// They check the bytes actually decompressed, not only the sizes an archive
// declares.
type Limits struct {
	// MaxEntries is the most central directory entries an archive may have.
	MaxEntries int
	// MaxEntryBytes is the largest uncompressed size of one entry.
	MaxEntryBytes uint64
	// MaxTotalBytes is the largest total uncompressed size of all entries.
	MaxTotalBytes uint64
	// MaxRatio is the highest uncompressed:compressed ratio allowed for an
	// entry larger than RatioAppliesAbove bytes.
	MaxRatio          uint64
	RatioAppliesAbove uint64
	// MaxManifestBytes is the largest manifest.json this library parses.
	MaxManifestBytes uint64
}

// DefaultLimits returns the limits every archive is read with. They match
// the reference limits of the specification's conformance fixtures and are
// not configurable.
func DefaultLimits() Limits { return defaultLimits }

var defaultLimits = Limits{
	MaxEntries:        100_000,
	MaxEntryBytes:     1 << 30, // 1 GiB
	MaxTotalBytes:     4 << 30, // 4 GiB
	MaxRatio:          1000,
	RatioAppliesAbove: 1 << 20, // 1 MiB
	MaxManifestBytes:  1 << 20, // 1 MiB
}

// limits is what this package applies; tests may lower it.
var limits = defaultLimits

// EntryKind says what an archive entry is.
type EntryKind int

const (
	// KindFile is a regular file.
	KindFile EntryKind = iota
	// KindDir is a directory entry (its stored name ends in "/").
	KindDir
	// KindSpecial is a symbolic link or other special file. It is never
	// followed or read (Section 13.1); see WarnSpecialEntry.
	KindSpecial
)

// Entry describes one archive entry.
type Entry struct {
	// Name is the stored path, exactly as in the archive. Directory entries
	// end in "/".
	Name string
	Kind EntryKind
	// Size is the uncompressed size in bytes (as declared, for an entry read
	// from an archive).
	Size     uint64
	Modified time.Time
}

type entry struct {
	name string // stored name; directories end in "/"
	key  string // NFC name without a trailing "/"
	kind EntryKind
	zf   *zip.File // the original entry, or nil for a new one
	data []byte    // new content; nil means the original's
	mod  time.Time
}

func (e *entry) size() uint64 {
	if e.data != nil || e.zf == nil {
		return uint64(len(e.data))
	}
	return e.zf.UncompressedSize64
}

// Archive is an MDZip archive held in memory. Open one with [Open] or
// [OpenBytes], or start a new one with [New]. An Archive is not safe for
// concurrent use.
type Archive struct {
	entries  []*entry
	manifest *Manifest
	warnings []Finding

	created bool   // made by New: saving is a creation
	baseEP  string // the entry point when opened
	baseErr error  // why it didn't resolve, if it didn't
	epSet   bool   // SetEntryPoint was called
	dirty   bool   // an entry was added, replaced or removed
	now     func() time.Time
}

// Open reads the archive at path. See [OpenBytes].
func Open(path string) (*Archive, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ErrIO, Path: path, Message: "can't read the file", Err: err}
	}
	return OpenBytes(data)
}

// OpenBytes reads an archive from memory. The Archive keeps data, so the
// caller must not change it afterwards.
//
// OpenBytes checks the whole container and manifest and fails with the
// first error: ErrZipInvalid, ErrZipEncrypted, ErrPathInvalid,
// ErrDuplicatePath, ErrPathConflict, ErrLimitExceeded, ErrManifestInvalid,
// ErrVersionUnsupported, ErrModeUnsupported or ErrEntryPointInvalid.
//
// An entry point that names a missing file, or that can't be determined, is
// not an open error: [Archive.EntryPoint] reports it, so an application can
// still show the files or repair the archive.
func OpenBytes(data []byte) (*Archive, error) {
	s := scan(data)
	if err := s.firstError(); err != nil {
		return nil, err
	}
	a := &Archive{entries: s.entries, manifest: s.manifest, warnings: s.warnings(), now: time.Now}
	a.baseEP, a.baseErr = a.EntryPoint()
	return a, nil
}

// scanResult is what checking an archive found.
type scanResult struct {
	entries  []*entry
	manifest *Manifest
	findings []Finding
}

func (s *scanResult) fail(e *Error) {
	s.findings = append(s.findings, Finding{Severity: SeverityError, Code: e.Code, Detail: e.Detail,
		Path: e.Path, Message: e.Message, err: e})
}

func (s *scanResult) warn(code Code, path, msg string) {
	s.findings = append(s.findings, Finding{Severity: SeverityWarning, Code: code, Path: path, Message: msg})
}

func (s *scanResult) hasErrors() bool {
	for _, f := range s.findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (s *scanResult) firstError() error {
	for _, f := range s.findings {
		if f.Severity == SeverityError {
			return f.err
		}
	}
	return nil
}

func (s *scanResult) warnings() []Finding {
	var w []Finding
	for _, f := range s.findings {
		if f.Severity == SeverityWarning {
			w = append(w, f)
		}
	}
	return w
}

// fileType is the Unix file type in an entry's external attributes.
func fileType(f *zip.File) uint32 { return (f.ExternalAttrs >> 16) & 0xF000 }

// scan checks an archive against Sections 4, 5.4, 6 and 13.1.
func scan(data []byte) *scanResult {
	s := &scanResult{}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		// ErrInsecurePath only flags names; Section 5.4 checks them below.
		s.fail(&Error{Code: ErrZipInvalid, Message: "not a valid ZIP archive", Err: err})
		return s
	}
	if len(zr.File) > limits.MaxEntries {
		s.fail(newError(ErrLimitExceeded, "", "the archive has more entries than the limit"))
		return s
	}

	var total uint64
	for _, f := range zr.File {
		e := &entry{name: f.Name, zf: f, mod: f.Modified}
		switch {
		case strings.HasSuffix(f.Name, "/"):
			e.kind = KindDir
		case fileType(f) != 0 && fileType(f) != 0x8000 && fileType(f) != 0x4000:
			e.kind = KindSpecial // symbolic link, device, FIFO or socket
		}
		if !utf8.ValidString(f.Name) {
			s.fail(&Error{Code: ErrPathInvalid, Path: f.Name, Message: "entry name is not valid UTF-8"})
			continue
		}
		e.key = nfc(strings.TrimSuffix(f.Name, "/"))
		if f.Flags&0x1 != 0 {
			s.fail(newError(ErrZipEncrypted, f.Name, "entry is encrypted"))
			continue
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			s.fail(newError(ErrZipInvalid, f.Name, "entry uses compression method "+strconv.Itoa(int(f.Method))+
				"; only 0 (stored) and 8 (deflate) are allowed"))
			continue
		}
		if err := ValidatePath(f.Name); err != nil {
			s.fail(err.(*Error))
			continue
		}
		if e.kind == KindSpecial {
			s.warn(WarnSpecialEntry, f.Name, "entry is a symbolic link or other special file; it is not followed or read")
		}
		if e.kind != KindDir {
			u, c := f.UncompressedSize64, f.CompressedSize64
			if u > limits.MaxEntryBytes {
				s.fail(newError(ErrLimitExceeded, f.Name, "entry is larger than the size limit"))
			} else if u > limits.RatioAppliesAbove && (c == 0 || u/c > limits.MaxRatio) {
				s.fail(newError(ErrLimitExceeded, f.Name, "entry's compression ratio is above the limit"))
			}
			total += u
		}
		s.entries = append(s.entries, e)
	}
	if total > limits.MaxTotalBytes {
		s.fail(newError(ErrLimitExceeded, "", "the entries' total size is larger than the limit"))
	}
	if s.hasErrors() {
		return s
	}
	checkOverlap(s, zr)
	checkNames(s)
	if s.hasErrors() {
		return s
	}

	// The manifest (Section 6).
	if me := findEntry(s.entries, "manifest.json"); me != nil {
		switch me.kind {
		case KindFile:
			if me.zf.UncompressedSize64 > limits.MaxManifestBytes {
				s.fail(newError(ErrLimitExceeded, "manifest.json", "manifest.json is larger than the limit"))
				return s
			}
			raw, err := readZipFile(me.zf)
			if err != nil {
				s.fail(err.(*Error))
				return s
			}
			m, err := ParseManifest(raw)
			if err != nil {
				s.fail(err.(*Error))
				return s
			}
			m.dirty = false
			s.manifest = m
			s.findings = append(s.findings, m.warnings...)
			if c := m.Cover(); c != "" {
				if ce := findEntry(s.entries, c); ce == nil || ce.kind != KindFile {
					s.warn(WarnCoverMissing, c, "the cover image is not in the archive")
				}
			}
		case KindSpecial:
			// Reported above; a link is never read as the manifest.
		}
	}
	return s
}

// checkOverlap rejects entries whose data shares bytes with another entry's,
// which a ZIP bomb uses to declare far more data than the file holds.
func checkOverlap(s *scanResult, zr *zip.Reader) {
	type span struct {
		start, end int64
		name       string
	}
	spans := make([]span, 0, len(zr.File))
	for _, f := range zr.File {
		off, err := f.DataOffset()
		if err != nil {
			s.fail(&Error{Code: ErrZipInvalid, Path: f.Name, Message: "can't read the entry's local header", Err: err})
			return
		}
		spans = append(spans, span{off, off + int64(f.CompressedSize64), f.Name})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start == spans[i-1].start || spans[i].start < spans[i-1].end {
			s.fail(newError(ErrZipInvalid, spans[i].name, "entry shares data with "+spans[i-1].name))
			return
		}
	}
}

// checkNames applies the whole-archive rules of Section 5.4: no NFC
// duplicates, no file that is also a directory. Case-only differences are a
// warning.
func checkNames(s *scanResult) {
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, e := range s.entries {
		set := files
		if e.kind == KindDir {
			set = dirs
		}
		if set[e.key] {
			s.fail(newError(ErrDuplicatePath, e.name, "two entries have this path after NFC normalization"))
			continue
		}
		set[e.key] = true
	}
	parents := map[string]bool{}
	for _, e := range s.entries {
		for i := strings.IndexByte(e.key, '/'); i >= 0; i = nextSlash(e.key, i) {
			parents[e.key[:i]] = true
		}
	}
	folded := map[string]string{}
	for _, e := range s.entries {
		if e.kind == KindDir {
			continue
		}
		if dirs[e.key] || parents[e.key] {
			s.fail(newError(ErrPathConflict, e.name, "this file's path is also a directory"))
			continue
		}
		f := caseFold(e.key)
		if other, ok := folded[f]; ok {
			s.warn(WarnCaseCollision, e.name, "differs only in case from "+other+
				"; they collide on a case-insensitive filesystem")
		} else {
			folded[f] = e.name
		}
	}
}

func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// findEntry finds a file, special or directory entry by path, matched after
// NFC normalization and case-sensitively (Section 5.4.1).
func findEntry(entries []*entry, path string) *entry {
	key := nfc(strings.TrimSuffix(path, "/"))
	for _, e := range entries {
		if e.key == key && (e.kind == KindDir) == strings.HasSuffix(path, "/") {
			return e
		}
	}
	return nil
}

// file finds a regular file entry.
func (a *Archive) file(path string) *entry {
	if strings.HasSuffix(path, "/") {
		return nil
	}
	e := findEntry(a.entries, path)
	if e == nil || e.kind != KindFile {
		return nil
	}
	return e
}

// readZipFile decompresses an entry, never returning or decompressing much
// more than its declared size, and checks its CRC.
func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, &Error{Code: ErrZipInvalid, Path: f.Name, Message: "can't read the entry", Err: err}
	}
	defer rc.Close()
	declared := f.UncompressedSize64
	var buf bytes.Buffer
	if declared < 1<<20 {
		buf.Grow(int(declared))
	}
	_, err = io.Copy(&buf, io.LimitReader(rc, int64(declared)+1))
	switch {
	case err == nil && uint64(buf.Len()) > declared:
		fallthrough
	case errors.Is(err, zip.ErrFormat) && uint64(buf.Len()) >= declared:
		// The data goes on past the declared size.
		return nil, newError(ErrLimitExceeded, f.Name, "entry decompresses to more than its declared size")
	case err != nil:
		return nil, &Error{Code: ErrZipInvalid, Path: f.Name, Message: "the entry's data is damaged", Err: err}
	}
	return buf.Bytes(), nil
}

// Entries lists the archive's entries in order: stored files, directories
// and special entries, then new ones.
func (a *Archive) Entries() []Entry {
	out := make([]Entry, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, Entry{Name: e.name, Kind: e.kind, Size: e.size(), Modified: e.mod})
	}
	return out
}

// Has reports whether a regular file exists at path, matched after NFC
// normalization and case-sensitively. It never matches a name that differs
// only in case.
func (a *Archive) Has(path string) bool { return a.file(path) != nil }

// ReadFile returns the bytes of the file at path. It fails with ErrNotFound
// when there is no such regular file (directories and special entries
// aren't files), and with ErrLimitExceeded or ErrZipInvalid when the entry's
// data is larger than declared or damaged.
func (a *Archive) ReadFile(path string) ([]byte, error) {
	e := a.file(path)
	if e == nil {
		return nil, newError(ErrNotFound, path, "no file with this path"+a.nearMatch(path))
	}
	if e.data != nil || e.zf == nil {
		return append([]byte(nil), e.data...), nil
	}
	return readZipFile(e.zf)
}

// ReadText returns the file at path as UTF-8 text, without a leading byte
// order mark. Line endings are returned as stored (LF or CRLF). It fails
// with ErrTextInvalid when the bytes aren't valid UTF-8.
func (a *Archive) ReadText(path string) (string, error) {
	b, err := a.ReadFile(path)
	if err != nil {
		return "", err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		return "", newError(ErrTextInvalid, path, "the file is not valid UTF-8 text")
	}
	return string(b), nil
}

// Manifest returns the archive's manifest, or nil when it has none. Changes
// made through it are written by the next save. See [Archive.EditManifest].
func (a *Archive) Manifest() *Manifest { return a.manifest }

// Mode returns the archive's mode: the manifest's, or ModeDocument when it
// declares none (Section 5.7.1). Both modes are supported: in project mode
// each Markdown file is a separate document, read on its own.
func (a *Archive) Mode() Mode {
	if a.manifest == nil {
		return ModeDocument
	}
	return a.manifest.Mode()
}

// Warnings returns what opening the archive found that doesn't stop it
// being used, such as symbolic link entries (WarnSpecialEntry) or ignored
// manifest fields.
func (a *Archive) Warnings() []Finding { return append([]Finding(nil), a.warnings...) }

// EntryPoint returns the stored path of the primary Markdown file, using the
// discovery algorithm of Section 5.5 on the archive as it is now.
//
// A manifest entryPoint that matches no file fails with
// ErrEntryPointMissing; it never falls back to index.md or another file. With
// no entryPoint, it uses index.md, then the only root-level Markdown file
// that isn't a support file (README, AGENTS, LICENSE, CHANGELOG,
// CONTRIBUTING), then the only root-level Markdown file. Otherwise it fails
// with ErrEntryPointUnresolved; list the Markdown files with [Archive.Entries]
// and let the user choose.
func (a *Archive) EntryPoint() (string, error) {
	if a.manifest != nil {
		if v, ok := a.manifest.obj.get("entryPoint"); ok {
			s, _ := v.(string)
			p, err := cleanEntryPoint(s)
			if err != nil {
				return "", err
			}
			if e := a.file(p); e != nil {
				return e.name, nil
			}
			return "", newError(ErrEntryPointMissing, p, "the manifest's entryPoint names no file in the archive"+a.nearMatch(p))
		}
	}
	if e := a.file("index.md"); e != nil {
		return e.name, nil
	}
	var content, all []string
	for _, e := range a.entries {
		if e.kind != KindFile || strings.Contains(e.key, "/") || !IsMarkdown(e.key) {
			continue
		}
		all = append(all, e.name)
		if !isSupportFile(e.key) {
			content = append(content, e.name)
		}
	}
	switch {
	case len(content) == 1:
		return content[0], nil
	case len(content) == 0 && len(all) == 1:
		return all[0], nil
	case len(all) == 0:
		return "", newError(ErrEntryPointUnresolved, "", "the archive has no root-level Markdown file and no entryPoint")
	}
	if len(content) == 0 {
		content = all
	}
	return "", newError(ErrEntryPointUnresolved, "", "several root-level Markdown files and no entryPoint: "+strings.Join(content, ", "))
}

// nearMatch names a file that differs from path only in case, for messages
// (Section 5.4.1). It is never used as the match.
func (a *Archive) nearMatch(path string) string {
	f := caseFold(nfc(path))
	for _, e := range a.entries {
		if e.kind == KindFile && caseFold(e.key) == f {
			return "; did you mean " + e.name + "?"
		}
	}
	return ""
}
