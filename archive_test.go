package mdzip

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func zipOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, n := range names {
		w, _ := zw.Create(n)
		w.Write([]byte("# " + n + "\n"))
	}
	zw.Close()
	return b.Bytes()
}

func TestMaxEntries(t *testing.T) {
	saved := limits
	defer func() { limits = saved }()
	limits.MaxEntries = 3
	if _, err := OpenBytes(zipOf(t, "a.md", "b.md", "c.md", "d.md")); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("got %v, want ERR_LIMIT_EXCEEDED", err)
	}
	if _, err := OpenBytes(zipOf(t, "a.md", "b.md", "c.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSaveFailureLeavesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.mdz")
	a := New()
	if err := a.WriteFile("index.md", []byte("# Doc\n")); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteFile("other.md", []byte("# Other\n")); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(p); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)

	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Remove("index.md"); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(p); !errors.Is(err, ErrEntryPointChanged) {
		t.Fatalf("Save after removing the entry point = %v, want ERR_ENTRYPOINT_CHANGED", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("a failed save changed the file")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}

	// Changing it on purpose works.
	if err := b.SetEntryPoint("other.md"); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(p); err != nil {
		t.Fatal(err)
	}
	c, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if ep, _ := c.EntryPoint(); ep != "other.md" {
		t.Fatalf("entry point = %q", ep)
	}
	if _, ok := c.Manifest().Lookup("/modified/when"); !ok {
		t.Error("modified.when not set after a change")
	}
}

func TestWriteFileConflicts(t *testing.T) {
	a := New()
	if err := a.WriteFile("docs/intro.md", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteFile("docs", nil); !errors.Is(err, ErrPathConflict) {
		t.Errorf("file over a directory: %v", err)
	}
	if err := a.WriteFile("docs/intro.md/x.md", nil); !errors.Is(err, ErrPathConflict) {
		t.Errorf("file under a file: %v", err)
	}
	if err := a.WriteFile("../x.md", nil); !errors.Is(err, ErrPathTraversal) {
		t.Errorf("traversal: %v", err)
	}
}

func TestSetUnknownMode(t *testing.T) {
	m := newManifest()
	if err := m.Set("mode", "Document"); !errors.Is(err, ErrModeUnsupported) {
		t.Fatalf("got %v", err)
	}
	if _, ok := m.Lookup("/mode"); ok {
		t.Fatal("invalid mode was kept")
	}
}

func TestErrorChecks(t *testing.T) {
	_, err := New().ReadFile("missing.md")
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, os.ErrNotExist) || CodeOf(err) != ErrNotFound {
		t.Fatalf("error checks fail for %v", err)
	}
}
