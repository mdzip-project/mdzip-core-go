package mdzip_test

// Conformance tests: every consumer test (T-001 onward) and producer check
// (P-001 onward) from the specification's fixtures, run through the public
// API. The fixtures are copied from a released spec tag into
// testdata/fixtures; see TestFixturesUnchanged.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	mdzip "github.com/mdzip-project/mdzip-core-go"
)

const fixtures = "testdata/fixtures"

// ---- result table ----

type result struct{ id, status, note string }

var (
	resultsMu sync.Mutex
	results   []result
)

// conformance runs one fixture test as a subtest and records its outcome.
func conformance(t *testing.T, id string, fn func(t *testing.T)) {
	t.Helper()
	t.Run(id, func(t *testing.T) {
		t.Cleanup(func() {
			status := "passed"
			switch {
			case t.Failed():
				status = "FAILED"
			case t.Skipped():
				status = "not applicable"
			}
			resultsMu.Lock()
			results = append(results, result{id, status, skipNote[t.Name()]})
			resultsMu.Unlock()
		})
		fn(t)
	})
}

var skipNote = map[string]string{}

func notApplicable(t *testing.T, why string) {
	t.Helper()
	resultsMu.Lock()
	skipNote[t.Name()] = why
	resultsMu.Unlock()
	t.Skip(why)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if len(results) > 0 {
		sort.Slice(results, func(i, j int) bool { return idLess(results[i].id, results[j].id) })
		fmt.Println("\nConformance results (spec fixtures):")
		for _, r := range results {
			line := fmt.Sprintf("  %-6s %s", r.id, r.status)
			if r.note != "" {
				line += " — " + r.note
			}
			fmt.Println(line)
		}
	}
	os.Exit(code)
}

func idLess(a, b string) bool {
	if a[0] != b[0] {
		return a[0] > b[0] // T before P
	}
	return a < b
}

// ---- helpers ----

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(readFixture(t, name), v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// rawZip builds an archive directly with archive/zip, so tests can store
// names and content the library itself would never write.
func rawZip(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f[0], Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f[1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func open(t *testing.T, data []byte) *mdzip.Archive {
	t.Helper()
	a, err := mdzip.OpenBytes(data)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	return a
}

func entryPoint(t *testing.T, a *mdzip.Archive) string {
	t.Helper()
	ep, err := a.EntryPoint()
	if err != nil {
		t.Fatalf("EntryPoint: %v", err)
	}
	return ep
}

func save(t *testing.T, a *mdzip.Archive) []byte {
	t.Helper()
	b, err := a.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	checkProducedArchive(t, b, false)
	return b
}

func jsonEqual(a, b any) bool {
	norm := func(v any) any {
		raw, _ := json.Marshal(v)
		var out any
		json.Unmarshal(raw, &out)
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// checkProducedArchive applies P-007 to an archive the library wrote: methods
// 0 or 8, the UTF-8 flag on non-ASCII names, valid unique names, no symbolic
// links (unless kept from the input), a manifest without duplicate members,
// no ZIP64 records, and a clean `mdz validate` when the CLI is installed.
func checkProducedArchive(t *testing.T, data []byte, keptSymlinks bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("P-007: output is not a ZIP: %v", err)
	}
	for _, f := range zr.File {
		if f.Method != zip.Store && f.Method != zip.Deflate {
			t.Errorf("P-007: %s uses method %d", f.Name, f.Method)
		}
		ascii := true
		for i := 0; i < len(f.Name); i++ {
			if f.Name[i] >= 0x80 {
				ascii = false
			}
		}
		if !ascii && f.Flags&0x800 == 0 {
			t.Errorf("P-007: %s has non-ASCII characters but no UTF-8 flag", f.Name)
		}
		if f.Flags&0x8 != 0 {
			t.Errorf("P-007: %s uses a data descriptor", f.Name)
		}
		if typ := (f.ExternalAttrs >> 16) & 0xF000; typ == 0xA000 && !keptSymlinks {
			t.Errorf("P-007: %s is a symbolic link", f.Name)
		}
		for ex := f.Extra; len(ex) >= 4; {
			id, size := int(ex[0])|int(ex[1])<<8, int(ex[2])|int(ex[3])<<8
			if id == 0x0001 {
				t.Errorf("P-007: %s has a ZIP64 extra field", f.Name)
			}
			if 4+size > len(ex) {
				break
			}
			ex = ex[4+size:]
		}
	}
	if bytes.Contains(data, []byte("PK\x06\x06")) || bytes.Contains(data, []byte("PK\x06\x07")) {
		t.Errorf("P-007: output contains ZIP64 end records")
	}
	if r := mdzip.Validate(data); !r.Valid() {
		t.Errorf("P-007: output fails Validate: %+v", r.Errors())
	}
	if f, err := zr.Open("manifest.json"); err == nil {
		raw := new(bytes.Buffer)
		raw.ReadFrom(f)
		m, err := mdzip.ParseManifest(raw.Bytes())
		if err != nil {
			t.Errorf("P-007: manifest: %v", err)
		} else {
			for _, w := range m.Warnings() {
				if w.Code == mdzip.WarnDuplicateMember {
					t.Errorf("P-007: manifest has a duplicate member %s", w.Path)
				}
			}
		}
	}
	mdzValidate(t, data, keptSymlinks)
}

// mdzValidate runs the mdz CLI's validator on data, if mdz is installed.
// With MDZ_REQUIRED=1 (as in CI), a missing mdz fails the test.
func mdzValidate(t *testing.T, data []byte, allowWarnings bool) {
	t.Helper()
	exe, err := exec.LookPath("mdz")
	if err != nil {
		if os.Getenv("MDZ_REQUIRED") == "1" {
			t.Fatal("MDZ_REQUIRED=1 but mdz is not on the PATH")
		}
		return
	}
	p := filepath.Join(t.TempDir(), "out.mdz")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(exe, "validate", p).CombinedOutput()
	text := string(out)
	switch {
	case err == nil && strings.Contains(text, "Status: valid"):
	case err == nil && allowWarnings && strings.Contains(text, "Status: warning"):
		t.Logf("mdz validate (warnings expected):\n%s", text)
	default:
		t.Errorf("mdz validate didn't pass cleanly (%v):\n%s", err, text)
	}
}

// ---- fixture integrity ----

func TestFixturesUnchanged(t *testing.T) {
	var marker struct {
		Ref   string            `json:"ref"`
		Files map[string]string `json:"files"`
	}
	loadJSON(t, "SPEC-FIXTURES.json", &marker)
	for name, want := range marker.Files {
		if got := sha(readFixture(t, name)); got != want {
			t.Errorf("%s differs from spec %s; resync the fixtures instead of editing them", name, marker.Ref)
		}
	}
	t.Logf("fixtures from spec %s (%d files)", marker.Ref, len(marker.Files))
}

// ---- consumer tests ----

func TestT001MinimalPackage(t *testing.T) {
	conformance(t, "T-001", func(t *testing.T) {
		a := open(t, readFixture(t, "minimal.mdz"))
		if ep := entryPoint(t, a); ep != "index.md" {
			t.Errorf("entry point = %q, want index.md", ep)
		}
		if _, err := a.ReadText("index.md"); err != nil {
			t.Error(err)
		}
	})
}

func TestT002WithManifest(t *testing.T) {
	conformance(t, "T-002", func(t *testing.T) {
		a := open(t, readFixture(t, "with-manifest.mdz"))
		if a.Manifest() == nil || a.Manifest().EntryPoint() != "index.md" {
			t.Fatal("manifest entryPoint not read")
		}
		if ep := entryPoint(t, a); ep != "index.md" {
			t.Errorf("entry point = %q", ep)
		}
	})
}

func TestT003T004EntryPointMissing(t *testing.T) {
	for _, c := range []struct{ id, file string }{
		{"T-003", "invalid-missing-index.mdz"},
		{"T-004", "invalid-bad-entrypoint.mdz"},
	} {
		conformance(t, c.id, func(t *testing.T) {
			a := open(t, readFixture(t, c.file))
			ep, err := a.EntryPoint()
			if !errors.Is(err, mdzip.ErrEntryPointMissing) {
				t.Fatalf("EntryPoint = %q, %v; want ERR_ENTRYPOINT_MISSING and no fallback", ep, err)
			}
			if r := mdzip.Validate(readFixture(t, c.file)); r.Valid() {
				t.Error("Validate reports the archive valid")
			}
		})
	}
}

func TestT005CRLF(t *testing.T) {
	conformance(t, "T-005", func(t *testing.T) {
		a := open(t, readFixture(t, "line-endings-crlf.mdz"))
		text, err := a.ReadText(entryPoint(t, a))
		if err != nil || !strings.Contains(text, "\r\n") {
			t.Fatalf("ReadText = %q, %v", text, err)
		}
	})
}

type entryPointCase struct {
	Name               string          `json:"name"`
	Files              []string        `json:"files"`
	Manifest           json.RawMessage `json:"manifest"`
	ManifestJSON       *string         `json:"manifestJson"`
	ExpectedEntryPoint *string         `json:"expectedEntryPoint"`
	ExpectedError      string          `json:"expectedError"`
}

func runEntryPointCase(t *testing.T, c entryPointCase) {
	var files [][2]string
	for _, f := range c.Files {
		content := "# " + f + "\n"
		if f == "manifest.json" {
			content = string(c.Manifest)
			if c.ManifestJSON != nil {
				content = *c.ManifestJSON
			}
		}
		files = append(files, [2]string{f, content})
	}
	a, err := mdzip.OpenBytes(rawZip(t, files...))
	var ep string
	if err == nil {
		ep, err = a.EntryPoint()
	}
	switch {
	case c.ExpectedError != "":
		if mdzip.CodeOf(err) != mdzip.Code(c.ExpectedError) {
			t.Errorf("%s: got %q, %v; want %s", c.Name, ep, err, c.ExpectedError)
		}
	case c.ExpectedEntryPoint != nil:
		if err != nil || ep != *c.ExpectedEntryPoint {
			t.Errorf("%s: got %q, %v; want %q", c.Name, ep, err, *c.ExpectedEntryPoint)
		}
	default:
		if !errors.Is(err, mdzip.ErrEntryPointUnresolved) {
			t.Errorf("%s: got %q, %v; want no entry point (ERR_ENTRYPOINT_UNRESOLVED)", c.Name, ep, err)
		}
	}
}

func TestT006T008EntryPointCases(t *testing.T) {
	var cases []entryPointCase
	loadJSON(t, "entrypoint-cases.json", &cases)
	conformance(t, "T-006", func(t *testing.T) {
		for _, c := range cases {
			if c.ExpectedError == "" {
				runEntryPointCase(t, c)
			}
		}
	})
	conformance(t, "T-008", func(t *testing.T) {
		for _, c := range cases {
			if c.ExpectedError != "" {
				runEntryPointCase(t, c)
			}
		}
	})
}

func TestT007NoManifest(t *testing.T) {
	conformance(t, "T-007", func(t *testing.T) {
		data := readFixture(t, "minimal.mdz")
		a := open(t, data)
		if a.Manifest() != nil || len(a.Warnings()) != 0 {
			t.Errorf("manifest = %v, warnings = %v", a.Manifest(), a.Warnings())
		}
		if r := mdzip.Validate(data); !r.Valid() || len(r.Findings) != 0 {
			t.Errorf("Validate findings for a manifest-less archive: %+v", r.Findings)
		}
	})
}

func TestT009PathMatching(t *testing.T) {
	var cases []struct {
		Name    string `json:"name"`
		Entry   string `json:"entry"`
		Lookup  string `json:"lookup"`
		Matches bool   `json:"matches"`
	}
	loadJSON(t, "path-matching-cases.json", &cases)
	conformance(t, "T-009", func(t *testing.T) {
		for _, c := range cases {
			a := open(t, rawZip(t, [2]string{c.Entry, "x"}))
			if got := a.Has(c.Lookup); got != c.Matches {
				t.Errorf("%s: Has(%q) with entry %q = %v, want %v", c.Name, c.Lookup, c.Entry, got, c.Matches)
			}
			_, err := a.ReadFile(c.Lookup)
			if (err == nil) != c.Matches {
				t.Errorf("%s: ReadFile error = %v", c.Name, err)
			}
		}
	})
}

func TestT010TurkishLocale(t *testing.T) {
	conformance(t, "T-010", func(t *testing.T) {
		// Go's string functions never consult the process locale; set it
		// anyway so the test matches the fixture's setup.
		t.Setenv("LC_ALL", "tr_TR.UTF-8")
		t.Setenv("LANG", "tr_TR.UTF-8")
		a := open(t, rawZip(t, [2]string{"LICENSE.md", "MIT"}, [2]string{"notes.md", "# Notes"}))
		if ep := entryPoint(t, a); ep != "notes.md" {
			t.Errorf("entry point = %q, want notes.md", ep)
		}
	})
}

func TestT011LinkResolution(t *testing.T) {
	var file struct {
		Host struct {
			Archives map[string]struct {
				Entries []string `json:"entries"`
			} `json:"archives"`
		} `json:"host"`
		Cases []struct {
			Name string `json:"name"`
			From struct {
				Archive string `json:"archive"`
				Path    string `json:"path"`
				Host    string `json:"host"`
			} `json:"from"`
			Link     string `json:"link"`
			Expected struct {
				Kind     string `json:"kind"`
				Entry    string `json:"entry"`
				Fragment string `json:"fragment"`
			} `json:"expected"`
		} `json:"cases"`
	}
	loadJSON(t, "link-resolution-cases.json", &file)
	archives := map[string]*mdzip.Archive{}
	for name, spec := range file.Host.Archives {
		var files [][2]string
		for _, e := range spec.Entries {
			files = append(files, [2]string{e, "x"})
		}
		archives[name] = open(t, rawZip(t, files...))
	}
	conformance(t, "T-011", func(t *testing.T) {
		ran, skipped := 0, 0
		for _, c := range file.Cases {
			if c.From.Archive == "" {
				skipped++ // resolving from a host file needs host support (Section 9.5)
				continue
			}
			ran++
			link, err := archives[c.From.Archive].Resolve(c.From.Path, c.Link)
			if err != nil {
				t.Errorf("%s: %v", c.Name, err)
				continue
			}
			// Links outside and into archives are optional and not supported
			// here, so the fixture's host and archive results must come back
			// unresolved (run with outsideLinks and intoArchiveLinks false).
			want := c.Expected.Kind
			if want == "host" || want == "archive" {
				want = "unresolved"
			}
			got := map[mdzip.LinkKind]string{mdzip.LinkInternal: "internal", mdzip.LinkURL: "url",
				mdzip.LinkUnresolved: "unresolved"}[link.Kind]
			if got != want || (want == "internal" && (link.Entry != c.Expected.Entry || link.Fragment != c.Expected.Fragment)) {
				t.Errorf("%s: %q → %s %q #%q (%s); want %s %q #%q", c.Name, c.Link, got, link.Entry,
					link.Fragment, link.Reason, want, c.Expected.Entry, c.Expected.Fragment)
			}
		}
		t.Logf("%d cases from documents in archives run; %d cases from host files not applicable", ran, skipped)
	})
}

func TestT012ManifestValidation(t *testing.T) {
	var file struct {
		Cases []struct {
			Name         string `json:"name"`
			ManifestJSON string `json:"manifestJson"`
			Expected     struct {
				Valid            bool           `json:"valid"`
				ErrorCode        string         `json:"errorCode"`
				IgnoredFields    []string       `json:"ignoredFields"`
				Values           map[string]any `json:"values"`
				DuplicateMembers bool           `json:"duplicateMembers"`
			} `json:"expected"`
		} `json:"cases"`
	}
	loadJSON(t, "manifest-cases.json", &file)
	conformance(t, "T-012", func(t *testing.T) {
		for _, c := range file.Cases {
			m, err := mdzip.ParseManifest([]byte(c.ManifestJSON))
			if !c.Expected.Valid {
				if mdzip.CodeOf(err) != mdzip.Code(c.Expected.ErrorCode) {
					t.Errorf("%s: got %v, want %s", c.Name, err, c.Expected.ErrorCode)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", c.Name, err)
				continue
			}
			var ignored []string
			dup := false
			for _, w := range m.Warnings() {
				switch w.Code {
				case mdzip.WarnFieldIgnored:
					ignored = append(ignored, w.Path)
				case mdzip.WarnDuplicateMember:
					dup = true
				}
			}
			sort.Strings(ignored)
			want := append([]string(nil), c.Expected.IgnoredFields...)
			sort.Strings(want)
			if strings.Join(ignored, ",") != strings.Join(want, ",") {
				t.Errorf("%s: ignored %v, want %v", c.Name, ignored, want)
			}
			if dup != c.Expected.DuplicateMembers {
				t.Errorf("%s: duplicate members reported = %v", c.Name, dup)
			}
			for ptr, v := range c.Expected.Values {
				got, ok := m.Lookup(ptr)
				if !ok || !jsonEqual(got, v) {
					t.Errorf("%s: %s = %v, want %v", c.Name, ptr, got, v)
				}
			}
		}
	})
}

func TestT013EntrySets(t *testing.T) {
	var file struct {
		Cases []struct {
			Name      string   `json:"name"`
			Entries   []string `json:"entries"`
			Valid     bool     `json:"valid"`
			ErrorCode string   `json:"errorCode"`
		} `json:"cases"`
	}
	loadJSON(t, "entry-set-cases.json", &file)
	conformance(t, "T-013", func(t *testing.T) {
		for _, c := range file.Cases {
			var files [][2]string
			for _, e := range c.Entries {
				files = append(files, [2]string{e, ""})
			}
			_, err := mdzip.OpenBytes(rawZip(t, files...))
			if c.Valid && err != nil {
				t.Errorf("%s: %v", c.Name, err)
			}
			if !c.Valid && mdzip.CodeOf(err) != mdzip.Code(c.ErrorCode) {
				t.Errorf("%s: got %v, want %s", c.Name, err, c.ErrorCode)
			}
		}
	})
}

func TestT014UnsupportedCompression(t *testing.T) {
	conformance(t, "T-014", func(t *testing.T) {
		_, err := mdzip.OpenBytes(readFixture(t, "unsupported-compression.mdz"))
		if !errors.Is(err, mdzip.ErrZipInvalid) {
			t.Fatalf("got %v, want ERR_ZIP_INVALID", err)
		}
	})
}

func TestT015ProjectMode(t *testing.T) {
	conformance(t, "T-015", func(t *testing.T) {
		a := open(t, readFixture(t, "project-mode.mdz"))
		if a.Mode() != mdzip.ModeProject {
			t.Fatalf("mode = %q", a.Mode())
		}
		if ep := entryPoint(t, a); ep != "index.md" {
			t.Errorf("entry point = %q", ep)
		}
		seen := map[string]string{}
		for _, doc := range []string{"index.md", "chapters/one.md", "chapters/two.md"} {
			text, err := a.ReadText(doc)
			if err != nil {
				t.Fatal(err)
			}
			for other, ot := range seen {
				if strings.Contains(text, ot) {
					t.Errorf("%s contains %s: documents joined", doc, other)
				}
			}
			seen[doc] = text
		}
	})
}

func TestT016HostileContent(t *testing.T) {
	conformance(t, "T-016", func(t *testing.T) {
		notApplicable(t, "for consumers that render; this library doesn't render Markdown")
	})
}

func TestT017SymlinkEntries(t *testing.T) {
	conformance(t, "T-017", func(t *testing.T) {
		a := open(t, readFixture(t, "symlink-entries.mdz"))
		if ep := entryPoint(t, a); ep != "index.md" {
			t.Errorf("entry point = %q", ep)
		}
		reported := map[string]bool{}
		for _, w := range a.Warnings() {
			if w.Code == mdzip.WarnSpecialEntry {
				reported[w.Path] = true
			}
		}
		for _, name := range []string{"shortcut.md", "linked", "escape.md"} {
			if !reported[name] {
				t.Errorf("symbolic link %s not reported", name)
			}
			if _, err := a.ReadFile(name); !errors.Is(err, mdzip.ErrNotFound) {
				t.Errorf("ReadFile(%s) = %v; a link must be skipped", name, err)
			}
		}
		for href, want := range map[string]string{"notes.md": "notes.md", "docs/page.md": "docs/page.md",
			"shortcut.md": "", "linked/page.md": "", "escape.md": ""} {
			link, err := a.Resolve("index.md", href)
			if err != nil {
				t.Fatal(err)
			}
			if want == "" && link.Kind != mdzip.LinkUnresolved || want != "" && link.Entry != want {
				t.Errorf("Resolve(%q) = %+v, want %q", href, link, want)
			}
		}
		t.Log("extraction: not applicable, the library has no extraction API")
	})
}

func TestT018ResourceLimits(t *testing.T) {
	conformance(t, "T-018", func(t *testing.T) {
		limitOrInvalid := func(err error) bool {
			return errors.Is(err, mdzip.ErrLimitExceeded) || errors.Is(err, mdzip.ErrZipInvalid)
		}
		// Forged size: opening may succeed (it declares 1 KiB); reading fails.
		a, err := mdzip.OpenBytes(readFixture(t, "limit-forged-size.mdz"))
		if err == nil {
			b, err := a.ReadFile("data/blob.bin")
			if !limitOrInvalid(err) || b != nil {
				t.Errorf("forged size: ReadFile = %d bytes, %v", len(b), err)
			}
			if _, err := a.ReadText("index.md"); err != nil {
				t.Errorf("forged size: index.md: %v", err)
			}
		} else if !limitOrInvalid(err) {
			t.Errorf("forged size: %v", err)
		}
		if _, err := mdzip.OpenBytes(readFixture(t, "limit-high-ratio.mdz")); !errors.Is(err, mdzip.ErrLimitExceeded) {
			t.Errorf("high ratio: %v, want ERR_LIMIT_EXCEEDED at open", err)
		}
		if _, err := mdzip.OpenBytes(readFixture(t, "limit-overlapping-entries.mdz")); !limitOrInvalid(err) {
			t.Errorf("overlapping entries: %v", err)
		}
	})
}

func TestPathValidationCases(t *testing.T) {
	var cases []struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		Valid     bool   `json:"valid"`
		ErrorCode string `json:"errorCode"`
	}
	loadJSON(t, "path-validation-cases.json", &cases)
	for _, c := range cases {
		err := mdzip.ValidatePath(c.Path)
		if c.Valid != (err == nil) {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if !c.Valid && !errors.Is(err, mdzip.Code(c.ErrorCode)) {
			t.Errorf("%s: got %v, want %s", c.Name, err, c.ErrorCode)
		}
	}
}

// ---- producer checks ----

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func TestP001NewArchive(t *testing.T) {
	conformance(t, "P-001", func(t *testing.T) {
		a := mdzip.New()
		a.EditManifest().SetApplication("conformance-test", "1.0.0", "")
		must(t, a.WriteFile("index.md", []byte("# Field guide\r\n\r\n![logo](images/logo.png)\r\n[Café](chapters/café.md)\r\n")))
		must(t, a.WriteFile("images/logo.png", png))
		must(t, a.WriteFile("chapters/café.md", []byte("# Café\n")))
		b := open(t, save(t, a))
		m := b.Manifest()
		if m == nil {
			t.Fatal("no manifest.json")
		}
		if m.SpecVersion() != mdzip.SpecVersion || m.EntryPoint() != "index.md" || !mdzip.IsMarkdown(m.EntryPoint()) || !b.Has(m.EntryPoint()) {
			t.Errorf("spec.version %q, entryPoint %q", m.SpecVersion(), m.EntryPoint())
		}
		if m.Title() != "Field guide" {
			t.Errorf("title = %q", m.Title())
		}
		if v, ok := m.Lookup("/mode"); !ok || v != "document" {
			t.Errorf("mode = %v", v)
		}
		if text, _ := b.ReadText("index.md"); strings.Contains(text, "\r") {
			t.Error("Markdown kept CRLF")
		}
	})
}

func TestP002P003AddSupportFile(t *testing.T) {
	for _, c := range []struct{ id, existing string }{{"P-002", "notes.md"}, {"P-003", "README.md"}} {
		conformance(t, c.id, func(t *testing.T) {
			a := open(t, rawZip(t, [2]string{c.existing, "# Doc\n"}))
			must(t, a.WriteFile("AGENTS.md", []byte("# Agents\n")))
			b := open(t, save(t, a))
			if ep := entryPoint(t, b); ep != c.existing {
				t.Errorf("entry point = %q, want %s", ep, c.existing)
			}
			if b.Manifest() == nil || b.Manifest().EntryPoint() != c.existing {
				t.Errorf("no manifest naming %s", c.existing)
			}
		})
	}
}

type roundTrip struct {
	PreservedManifestValues []struct {
		Pointer string `json:"pointer"`
		Value   any    `json:"value"`
	} `json:"preservedManifestValues"`
	PreservedEntries []struct {
		Path   string `json:"path"`
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"preservedEntries"`
	Flows []struct {
		Name    string   `json:"name"`
		Changes []string `json:"changes"`
		Adds    []string `json:"adds"`
		Removes []string `json:"removes"`
	} `json:"flows"`
}

func TestP004P005RoundTrip(t *testing.T) {
	var rt roundTrip
	loadJSON(t, "round-trip-expectations.json", &rt)
	run := func(t *testing.T, check func(t *testing.T, flow string, a *mdzip.Archive, changed map[string]bool)) {
		for _, flow := range rt.Flows {
			data := readFixture(t, "round-trip.mdz")
			changed := map[string]bool{}
			for _, p := range append(flow.Changes, flow.Removes...) {
				changed[p] = true
			}
			for i := 0; i < 2; i++ {
				a := open(t, data)
				switch flow.Name {
				case "change a recognized manifest field":
					a.Manifest().SetTitle("Edited title")
				case "edit document content":
					text, _ := a.ReadText("index.md")
					must(t, a.WriteFile("index.md", []byte(text+"\nAppended line.\n")))
				case "add a resource":
					must(t, a.WriteFile("images/added.png", png))
				case "remove a recognized resource":
					if a.Has("docs/guide.md") {
						must(t, a.Remove("docs/guide.md"))
					}
					text, _ := a.ReadText("index.md")
					var kept []string
					for _, l := range strings.Split(text, "\n") {
						if !strings.Contains(l, "docs/guide.md") {
							kept = append(kept, l)
						}
					}
					must(t, a.WriteFile("index.md", []byte(strings.Join(kept, "\n"))))
				case "open and save without an edit":
				default:
					t.Fatalf("unknown flow %q", flow.Name)
				}
				data = save(t, a)
				b := open(t, data)
				check(t, fmt.Sprintf("%s (save %d)", flow.Name, i+1), b, changed)
				for _, p := range flow.Adds {
					if !b.Has(p) {
						t.Errorf("%s: %s missing", flow.Name, p)
					}
				}
				for _, p := range flow.Removes {
					if b.Has(p) {
						t.Errorf("%s: %s still present", flow.Name, p)
					}
				}
			}
		}
	}
	conformance(t, "P-004", func(t *testing.T) {
		run(t, func(t *testing.T, flow string, a *mdzip.Archive, _ map[string]bool) {
			for _, pv := range rt.PreservedManifestValues {
				got, ok := a.Manifest().Lookup(pv.Pointer)
				if !ok || !jsonEqual(got, pv.Value) {
					t.Errorf("%s: %s = %v, want %v", flow, pv.Pointer, got, pv.Value)
				}
			}
		})
	})
	conformance(t, "P-005", func(t *testing.T) {
		run(t, func(t *testing.T, flow string, a *mdzip.Archive, changed map[string]bool) {
			for _, pe := range rt.PreservedEntries {
				if changed[pe.Path] {
					continue
				}
				b, err := a.ReadFile(pe.Path)
				if err != nil || len(b) != pe.Bytes || sha(b) != pe.SHA256 {
					t.Errorf("%s: %s changed (%d bytes, %v)", flow, pe.Path, len(b), err)
				}
			}
		})
	})
}

func TestP006CreateVersusModify(t *testing.T) {
	conformance(t, "P-006", func(t *testing.T) {
		// Creating always writes manifest.json.
		a := mdzip.New()
		must(t, a.WriteFile("index.md", []byte("# Hi\n")))
		if open(t, save(t, a)).Manifest() == nil {
			t.Error("creation wrote no manifest.json")
		}
		// Editing a manifest-less archive keeps its entry point.
		b := open(t, rawZip(t, [2]string{"notes.md", "# Notes\n"}, [2]string{"README.md", "# Read me\n"}))
		must(t, b.WriteFile("notes.md", []byte("# Notes, edited\n")))
		if ep := entryPoint(t, open(t, save(t, b))); ep != "notes.md" {
			t.Errorf("entry point after edit = %q", ep)
		}
	})
}

func TestP007EntryNamesAndFlags(t *testing.T) {
	conformance(t, "P-007", func(t *testing.T) {
		// checkProducedArchive runs P-007 on every archive the other producer
		// tests write; this adds an archive full of non-ASCII names.
		a := mdzip.New()
		for _, n := range []string{"index.md", "日本語/ノート.md", "Ελληνικά.md", "emoji/😀.png"} {
			must(t, a.WriteFile(n, []byte("# x\n")))
		}
		save(t, a)
	})
}

func TestP008ModifyWithSymlinks(t *testing.T) {
	conformance(t, "P-008", func(t *testing.T) {
		in := readFixture(t, "symlink-entries.mdz")
		a := open(t, in)
		n := 0
		for _, w := range a.Warnings() {
			if w.Code == mdzip.WarnSpecialEntry {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("%d symbolic links reported, want 3 (the user must be told)", n)
		}
		must(t, a.WriteFile("notes.md", []byte("# Notes, edited\n")))
		out, err := a.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		checkProducedArchive(t, out, true)
		links := func(data []byte) map[string]string {
			zr, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			m := map[string]string{}
			for _, f := range zr.File {
				if (f.ExternalAttrs>>16)&0xF000 == 0xA000 {
					rc, _ := f.Open()
					var b bytes.Buffer
					b.ReadFrom(rc)
					m[f.Name] = b.String()
				}
			}
			return m
		}
		if before, after := links(in), links(out); !reflect.DeepEqual(before, after) {
			t.Errorf("symbolic links changed: %v → %v", before, after)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
