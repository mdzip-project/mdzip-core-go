package mdzip

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Library identity, written to producer.core in archives this library
// creates (Section 14.1, item 16).
const (
	LibraryName    = "mdzip-core-go"
	LibraryVersion = "0.1.0"
	LibraryURL     = "https://github.com/mdzip-project/mdzip-core-go"
)

// New starts a new, empty archive. Add files with [Archive.WriteFile], set
// metadata with [Archive.EditManifest], then save it with [Archive.Save],
// [Archive.Bytes] or [Archive.WriteTo].
//
// Saving a new archive is a creation (Section 14.1): it always writes a
// manifest.json with spec.version, entryPoint, mode, title, created and
// producer.core. The entry point is the one set with [Archive.SetEntryPoint],
// or else the one discovery finds (index.md, or the only root-level Markdown
// file); saving fails with ErrEntryPointUnresolved when there is none. The
// title defaults to the entry point's first "# " heading, or its file name.
func New() *Archive {
	return &Archive{manifest: newManifest(), created: true, now: time.Now}
}

// EditManifest returns the manifest for editing, adding an empty one if the
// archive has none. Saving fills in what a conforming manifest needs.
func (a *Archive) EditManifest() *Manifest {
	if a.manifest == nil {
		a.manifest = newManifest()
	}
	return a.manifest
}

// WriteFile adds a file, or replaces the content of the file at that path.
//
// The path must satisfy Section 5.4 ([ValidatePath]) and name a file, not a
// directory. It is stored in NFC; its capitalization is kept. A file that
// would also be a directory fails with ErrPathConflict. Replacing an
// existing file keeps its stored name; replacing a symbolic link entry turns
// it into a regular file. Markdown files (.md, .markdown) are stored with LF
// line endings (Section 12.2); other files are stored as given.
//
// Writing manifest.json parses it as the new manifest; prefer
// [Archive.EditManifest], which keeps unknown fields for you.
func (a *Archive) WriteFile(name string, data []byte) error {
	if err := ValidatePath(name); err != nil {
		return err
	}
	if strings.HasSuffix(name, "/") {
		return newError(ErrPathInvalid, name, "WriteFile writes files; a name ending in \"/\" is a directory")
	}
	key := nfc(name)
	if key == "manifest.json" {
		m, err := ParseManifest(data)
		if err != nil {
			return err
		}
		m.dirty = true
		a.manifest = m
		return nil
	}
	if IsMarkdown(key) {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	data = append(make([]byte, 0, len(data)), data...) // also makes nil non-nil
	if e := findEntry(a.entries, key); e != nil {
		e.kind, e.data, e.mod = KindFile, data, a.now()
		a.dirty = true
		return nil
	}
	for _, e := range a.entries {
		switch {
		case e.key == key: // a directory entry with this path
			return newError(ErrPathConflict, name, "a directory has this path")
		case strings.HasPrefix(e.key, key+"/"):
			return newError(ErrPathConflict, name, "this path is a directory of "+e.name)
		case e.kind != KindDir && strings.HasPrefix(key, e.key+"/"):
			return newError(ErrPathConflict, name, "a parent directory of this path is the file "+e.name)
		}
	}
	a.entries = append(a.entries, &entry{name: key, key: key, kind: KindFile, data: data, mod: a.now()})
	a.dirty = true
	return nil
}

// Remove removes the entry at path: a file, a special entry such as a
// symbolic link, or a directory entry (name ending in "/"). Removing a
// directory entry doesn't remove the files under it. Removing manifest.json
// removes the manifest; saving then writes a new one. It fails with
// ErrNotFound when there is no such entry.
func (a *Archive) Remove(path string) error {
	if nfc(path) == "manifest.json" && a.manifest != nil {
		a.manifest = nil
		a.dirty = true
		if e := findEntry(a.entries, path); e != nil {
			a.drop(e)
		}
		return nil
	}
	e := findEntry(a.entries, path)
	if e == nil {
		return newError(ErrNotFound, path, "no entry with this path")
	}
	a.drop(e)
	a.dirty = true
	return nil
}

func (a *Archive) drop(e *entry) {
	for i, x := range a.entries {
		if x == e {
			a.entries = append(a.entries[:i], a.entries[i+1:]...)
			return
		}
	}
}

// SetEntryPoint makes path the entry point, writing it to the manifest's
// entryPoint (and adding a manifest if there is none). The path must be a
// valid entry point path; the file must exist when the archive is saved.
func (a *Archive) SetEntryPoint(path string) error {
	p, err := cleanEntryPoint(path)
	if err != nil {
		return err
	}
	if e := a.file(p); e != nil {
		p = e.name
	}
	a.EditManifest().obj.set("entryPoint", p)
	a.manifest.dirty = true
	a.epSet = true
	return nil
}

// Bytes returns the archive as an .mdz file. See [Archive.Save].
func (a *Archive) Bytes() ([]byte, error) {
	var b bytes.Buffer
	if err := a.write(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// WriteTo writes the archive as an .mdz file to w. Nothing is written when
// the archive can't be saved; see [Archive.Save].
func (a *Archive) WriteTo(w io.Writer) (int64, error) {
	b, err := a.Bytes()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	if err != nil {
		return int64(n), &Error{Code: ErrIO, Message: "writing the archive failed", Err: err}
	}
	return int64(n), nil
}

// Save writes the archive to path. It writes a temporary file next to path
// and renames it over the original only once it is complete, so a failed
// save leaves any existing file as it was.
//
// For an opened archive, saving is a modification (Section 14.1): every
// entry you didn't change keeps its path and bytes, unknown manifest fields
// are kept, and manifest modified.when is updated when something changed.
// Saving fails with ErrEntryPointChanged if your changes would change which
// file the entry point resolves to, or leave it unresolvable, unless you
// called [Archive.SetEntryPoint]. An archive without a manifest gets one
// naming the entry point it resolved to when opened. Symbolic link entries
// are kept as they are (see WarnSpecialEntry): remove them to repair the
// archive. Every untouched entry is decompressed and checked first, so a
// damaged entry fails the save with its error rather than being copied.
func (a *Archive) Save(path string) error {
	data, err := a.Bytes()
	if err != nil {
		return err
	}
	ioErr := func(msg string, err error) error {
		return &Error{Code: ErrIO, Path: path, Message: msg, Err: err}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return ioErr("can't create a temporary file", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return ioErr("writing the temporary file failed", err)
	}
	if err := tmp.Sync(); err != nil {
		return ioErr("writing the temporary file failed", err)
	}
	if err := tmp.Close(); err != nil {
		return ioErr("writing the temporary file failed", err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	_ = os.Chmod(tmpName, mode)
	if err := os.Rename(tmpName, path); err != nil {
		return ioErr("replacing the file failed", err)
	}
	ok = true
	return nil
}

// write produces the archive: it settles the entry point and manifest, then
// writes every entry.
func (a *Archive) write(w io.Writer) (err error) {
	now := a.now().UTC().Truncate(time.Second)
	if a.manifest == nil && !a.created && a.baseErr == nil {
		// Section 14.1: add a manifest naming the entry point the archive
		// resolved to before the change (adding a second support file, for
		// example, would otherwise make it ambiguous).
		m := newManifest()
		m.obj.set("entryPoint", a.baseEP)
		a.manifest = m
		defer func() {
			if a.manifest == m && err != nil {
				a.manifest = nil
			}
		}()
	}
	ep, epErr := a.EntryPoint()
	switch {
	case a.created || a.epSet:
		if epErr != nil {
			return epErr
		}
	case a.baseErr == nil && (epErr != nil || ep != a.baseEP):
		msg := "the change would leave the entry point unresolvable"
		if epErr == nil {
			msg = "the change would move the entry point from " + a.baseEP + " to " + ep
		}
		return &Error{Code: ErrEntryPointChanged, Path: a.baseEP, Message: msg +
			"; call SetEntryPoint to change it on purpose", Err: epErr}
	}

	m := a.manifest
	if m == nil && epErr == nil {
		m = newManifest() // Section 14.1: add one naming the entry point
	}
	if m != nil {
		if err := a.complete(m, ep, epErr, now); err != nil {
			return err
		}
	}

	zw := zip.NewWriter(w)
	if m != nil && (m.dirty || findEntry(a.entries, "manifest.json") == nil) {
		if findEntry(a.entries, "manifest.json") == nil {
			if err := writeNew(zw, "manifest.json", m.bytes(), now); err != nil {
				return err
			}
		}
	}
	for _, e := range a.entries {
		var err error
		switch {
		case e.key == "manifest.json" && e.kind == KindFile && m != nil && m.dirty:
			err = writeNew(zw, e.name, m.bytes(), now)
		case e.kind == KindDir:
			err = writeDir(zw, e)
		case e.zf != nil && e.data == nil:
			err = copyRaw(zw, e.zf)
		default:
			err = writeNew(zw, e.name, e.data, e.mod)
		}
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return &Error{Code: ErrIO, Message: "writing the archive failed", Err: err}
	}
	return nil
}

// complete fills in what the manifest must say before it is written.
func (a *Archive) complete(m *Manifest, ep string, epErr error, now time.Time) error {
	o := m.obj
	stamp := func(field string) {
		when := now.Format(time.RFC3339)
		if t, ok := o.get(field); ok {
			if to, isObj := t.(*object); isObj {
				to.set("when", when)
				return
			}
		}
		t := newObject()
		t.set("when", when)
		o.set(field, t)
	}
	fresh := a.created || m.isNew
	if fresh {
		spec := o.child("spec", true)
		if _, ok := spec.get("name"); !ok {
			spec.set("name", "mdzip-spec")
		}
		spec.set("version", SpecVersion)
		core := o.child("producer", true).child("core", true)
		core.set("name", LibraryName)
		core.set("version", LibraryVersion)
		core.set("url", LibraryURL)
		m.dirty = true
	}
	if epErr == nil && (fresh || a.epSet) {
		if cur, ok := o.get("entryPoint"); !ok || cur != ep {
			o.set("entryPoint", ep)
			m.dirty = true
		}
	}
	if _, ok := o.get("mode"); !ok && fresh {
		o.set("mode", string(ModeDocument))
	}
	if a.created {
		if m.Title() == "" && epErr == nil {
			o.set("title", a.deriveTitle(ep))
		}
		if _, ok := o.get("created"); !ok {
			stamp("created")
		}
	} else if a.dirty || m.dirty {
		stamp("modified")
		m.dirty = true
	}
	// Re-check: a caller may have set a critical field through Set.
	saved := m.warnings
	defer func() { m.warnings = saved }()
	return m.validate()
}

// deriveTitle returns the entry point's first ATX "# " heading, or its file
// name without the extension.
func (a *Archive) deriveTitle(ep string) string {
	if text, err := a.ReadText(ep); err == nil {
		fence := false
		for _, line := range strings.Split(text, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				fence = !fence
			}
			if !fence && strings.HasPrefix(t, "# ") {
				if h := strings.TrimSpace(strings.TrimRight(t[2:], "# ")); h != "" {
					return h
				}
			}
		}
	}
	base := path.Base(ep)
	return strings.TrimSuffix(base, path.Ext(base))
}

// header returns a FileHeader with the fields CreateRaw writes as given.
func header(name string, method uint16, mod time.Time) *zip.FileHeader {
	fh := &zip.FileHeader{Name: name, Method: method, ReaderVersion: 20}
	fh.CreatorVersion = 3<<8 | 20 // Unix, so the file mode below is read
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			fh.Flags |= 0x800 // UTF-8 name (Section 4)
			break
		}
	}
	fh.ModifiedDate, fh.ModifiedTime = msDosTime(mod)
	return fh
}

// writeNew compresses and writes a file entry. Each entry is written with
// its sizes and CRC up front, so there is no data descriptor and no ZIP64
// record unless a size needs one.
func writeNew(zw *zip.Writer, name string, data []byte, mod time.Time) error {
	method, body := uint16(zip.Store), data
	if len(data) > 0 {
		var buf bytes.Buffer
		fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		fw.Write(data)
		fw.Close()
		if buf.Len() < len(data) {
			method, body = zip.Deflate, buf.Bytes()
		}
	}
	fh := header(name, method, mod)
	fh.ExternalAttrs = 0o100644 << 16 // regular file, rw-r--r--
	fh.CRC32 = crc32.ChecksumIEEE(data)
	fh.CompressedSize64 = uint64(len(body))
	fh.UncompressedSize64 = uint64(len(data))
	return rawWrite(zw, fh, bytes.NewReader(body))
}

func writeDir(zw *zip.Writer, e *entry) error {
	fh := header(e.name, zip.Store, e.mod)
	fh.ExternalAttrs = (0o40755 << 16) | 0x10 // directory, plus the MS-DOS directory bit
	if e.zf != nil {
		fh.ExternalAttrs = e.zf.ExternalAttrs
	}
	return rawWrite(zw, fh, bytes.NewReader(nil))
}

// copyRaw copies an untouched entry's compressed bytes, after checking that
// they decompress to the declared size and CRC within the limits.
func copyRaw(zw *zip.Writer, f *zip.File) error {
	if _, err := readZipFile(f); err != nil {
		return err
	}
	fh := header(f.Name, f.Method, time.Time{})
	fh.ModifiedDate, fh.ModifiedTime = f.ModifiedDate, f.ModifiedTime
	fh.CreatorVersion = f.CreatorVersion&0xff00 | 20
	fh.ExternalAttrs = f.ExternalAttrs
	fh.CRC32 = f.CRC32
	fh.CompressedSize64 = f.CompressedSize64
	fh.UncompressedSize64 = f.UncompressedSize64
	r, err := f.OpenRaw()
	if err != nil {
		return &Error{Code: ErrZipInvalid, Path: f.Name, Message: "can't read the entry", Err: err}
	}
	return rawWrite(zw, fh, r)
}

func rawWrite(zw *zip.Writer, fh *zip.FileHeader, r io.Reader) error {
	w, err := zw.CreateRaw(fh)
	if err == nil {
		_, err = io.Copy(w, r)
	}
	if err != nil {
		return &Error{Code: ErrIO, Path: fh.Name, Message: "writing the entry failed", Err: err}
	}
	return nil
}

// msDosTime converts t to the MS-DOS date and time ZIP headers store.
func msDosTime(t time.Time) (date, tm uint16) {
	if t.IsZero() || t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	t = t.UTC()
	date = uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9)
	tm = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	return
}
