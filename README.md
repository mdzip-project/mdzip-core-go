# mdzip-core-go

A Go library for reading, writing and validating MDZip (`.mdz`) archives: ZIP files that hold one or more Markdown documents, the files they use and a `manifest.json`. It follows the [MDZip specification](https://github.com/mdzip-project/mdzip-spec) 1.2 and passes its conformance fixtures.

It doesn't render Markdown; your application does that.

```sh
go get github.com/mdzip-project/mdzip-core-go
```

Requires Go 1.26 or later. Dependencies: the standard library's `archive/zip`, plus `golang.org/x/text` for Unicode NFC normalization and case folding, which the standard library lacks.

## Example

```go
import mdzip "github.com/mdzip-project/mdzip-core-go"

// Create
a := mdzip.New()
a.EditManifest().SetApplication("My App", "1.0.0", "https://example.com")
a.WriteFile("index.md", []byte("# Field notes\n\n![map](images/map.png)\n"))
a.WriteFile("images/map.png", pngBytes)
if err := a.Save("notes.mdz"); err != nil { ... }

// Open and read
b, err := mdzip.Open("notes.mdz")
ep, err := b.EntryPoint()               // "index.md"
text, err := b.ReadText(ep)
link, err := b.Resolve(ep, "images/map.png") // LinkInternal, Entry "images/map.png"

// Modify
b.WriteFile("index.md", []byte(text+"\nMore notes.\n"))
b.Manifest().SetTitle("Field notes, revised")
err = b.Save("notes.mdz") // untouched entries keep their bytes

// Validate without opening
report, err := mdzip.ValidateFile("notes.mdz")
for _, f := range report.Findings { fmt.Println(f.Severity, f.Code, f.Path) }
```

## Contract operations (R1–R7)

How the [core library contract](https://github.com/mdzip-project/.github/blob/main/docs/core-contract.md) maps to this API:

| ID | Operation | API |
|---|---|---|
| R1 | Open/read an archive | `Open(path)`, `OpenBytes(data)`. The archive is read into memory. Opening checks the container and manifest and fails with the first error. |
| R2 | Inspect entries and metadata | `Archive.Entries()` (stored names; `KindFile`, `KindDir`, `KindSpecial`), `Has`, `ReadFile`, `ReadText`, `Manifest()` (typed getters plus `Lookup(jsonPointer)` for any field), `EntryPoint()`, `Mode()`, `Warnings()` |
| R3 | Resolve a link | `Archive.Resolve(from, dest)` → `Link{Kind, Entry, Fragment, Reason}` |
| R4 | Validate | `Validate(data)`, `ValidateFile(path)` → `Report` with every `Finding` (severity, code, path); `ParseManifest`, `ValidatePath` for single parts |
| R5 | Create an archive | `New()`, then `WriteFile`, `SetEntryPoint`, `EditManifest()`; write with `Save`, `Bytes` or `WriteTo` |
| R6 | Modify an archive | `WriteFile` (add or replace), `Remove`, `SetEntryPoint`, `Manifest()`/`EditManifest()` setters, `Set`, `Delete`; then `Save`, `Bytes` or `WriteTo` |
| R7 | Safety rules | Applied by `Open`, `OpenBytes`, `Validate` and every read: path rules, NFC duplicates and conflicts, compression methods, encryption, symbolic links, and the limits in `DefaultLimits()` |

## How it behaves

**Errors.** Every error is an `*mdzip.Error` with a stable `Code`, such as `ERR_ENTRYPOINT_MISSING`. Check it without reading the message:

```go
if errors.Is(err, mdzip.ErrEntryPointMissing) { ... }
switch mdzip.CodeOf(err) { case mdzip.ErrZipInvalid: ... }
```

The codes are those in Section 16.5 of the specification. A path error has `Code` `ERR_PATH_INVALID` and a `Detail` saying what's wrong (`ERR_PATH_TRAVERSAL`, `ERR_PATH_LEADING_SLASH`, `ERR_PATH_RESERVED_CHAR`, `ERR_PATH_CONTROL_CHAR`, `ERR_PATH_EMPTY`); `errors.Is` matches either. The library adds `ERR_ENTRYPOINT_CHANGED`, `ERR_NOT_FOUND` (also matches `fs.ErrNotExist`), `ERR_TEXT_INVALID` and `ERR_IO`. Warnings (`WARN_FIELD_IGNORED`, `WARN_DUPLICATE_MEMBER`, `WARN_SPECIAL_ENTRY`, `WARN_CASE_COLLISION`, `WARN_COVER_MISSING`, `WARN_VERSION_OLDER`) never stop an archive opening.

**Entry point.** `EntryPoint()` follows Section 5.5 on the archive as it is now. A manifest `entryPoint` that's invalid fails `Open` with `ERR_ENTRYPOINT_INVALID`. One that names a missing file returns `ERR_ENTRYPOINT_MISSING` from `EntryPoint()`, never a fallback to `index.md` or `README.md`, and an ambiguous archive returns `ERR_ENTRYPOINT_UNRESOLVED`. These two don't fail `Open`, so an application can still list the files, let the user choose, or repair the archive.

**Paths.** Lookups normalize to NFC and compare case-sensitively; a name that differs only in case never matches (the error message suggests it). Support file names and Markdown extensions are compared ASCII case-insensitively, never with locale rules.

**Modes.** Both modes are supported. In project mode each Markdown file is a document of its own; nothing joins them. An unknown mode fails `Open` with `ERR_MODE_UNSUPPORTED`.

**Links.** `Resolve` implements Section 9.3: fragment and query splitting, percent-decoding once per segment, and NFC case-sensitive matching. A `..` that climbs above the archive root is never clamped; the link is unresolved with `ReasonOutside`. Links outside and into archives (Sections 9.4–9.5) are optional and not resolved here; a host application can resolve them under Section 9.6.

**Symbolic links and special files** are never followed or read. They're listed by `Entries()` as `KindSpecial` and reported as `WARN_SPECIAL_ENTRY`. Saving keeps them unchanged, so tell the user; `Remove` repairs the archive.

**Limits.** Reading enforces at most 100,000 entries, 1 GiB per entry, 4 GiB in total and a 1000:1 compression ratio above 1 MiB, checked on declared sizes when opening and on the bytes actually decompressed when reading. An entry never returns more than its declared size; one that expands past it fails with `ERR_LIMIT_EXCEEDED`. Entries that share data fail with `ERR_ZIP_INVALID`.

**Saving.** `Save` writes a temporary file next to the target and renames it over the original only when it's complete, so a failed save leaves the original file as it was.
- **Creating** (`New`) always writes a manifest with `spec.version`, `entryPoint`, `mode`, `title` (from the first `# ` heading or the file name), `created` and `producer.core`.
- **Modifying** an opened archive keeps every untouched entry's path and bytes (copied without recompressing, after checking its CRC), keeps unknown manifest fields, and refreshes `modified.when`. It fails with `ERR_ENTRYPOINT_CHANGED` if a change would move or break the entry point, unless you called `SetEntryPoint`. An archive without a manifest gets one naming the entry point it had before the change.
- **Written entries** use stored or deflate only, have the UTF-8 flag on non-ASCII names, give sizes in the local header (no data descriptors), and carry no ZIP64 records unless a size needs one. Markdown files get LF line endings; other files are written as given.

## Conformance

The fixtures in `testdata/fixtures` are copied from spec tag `v1.2.1` with the spec's sync tool, and a test checks them against the `SPEC-FIXTURES.json` checksums. To update them:

```sh
node ../mdzip-spec/tools/sync-fixtures.mjs testdata/fixtures --ref <tag>
```

`go test -v ./...` runs every consumer test and producer check through the public API and prints a results table. When the [`mdz` CLI](https://mdzip.org/docs/cli.html) is on the PATH, every archive the tests write is also checked with `mdz validate`. Results against v1.2.1:

| Tests | Result |
|---|---|
| T-001 – T-015, T-017, T-018 | Passed |
| T-011 | Passed for the 42 cases resolved from documents inside an archive, run as a consumer without links outside or into archives (those must come back unresolved). The 11 cases that resolve from a host file are not applicable. |
| T-016 | Not applicable: it's for consumers that render Markdown |
| T-017 | Passed; its extraction part doesn't apply, since the library has no extraction API |
| P-001 – P-008 | Passed. `mdz validate` reports every archive the tests write as valid, except the P-008 output, which keeps the fixture's three symbolic links as the check allows, so `mdz` warns about them. |

## Not included

The contract's optional extensions are left out: image analysis, a workspace or editor model, asset helpers (MIME types, data URIs), packaging conveniences (folder scans, filters), configurable limits and host link resolution. Extraction to disk isn't provided either.

## License

Apache-2.0. See [LICENSE](LICENSE).
