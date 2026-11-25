// Package mdzip reads, writes and validates MDZip (.mdz) archives: ZIP files
// holding one or more Markdown documents, the files they use and a
// manifest.json, as defined by the MDZip specification
// (https://github.com/mdzip-project/mdzip-spec).
//
// Open an archive with [Open] or [OpenBytes], find its primary document with
// [Archive.EntryPoint], read entries with [Archive.ReadFile] and
// [Archive.ReadText], and resolve links between documents with
// [Archive.Resolve]. Start a new archive with [New]. Change an archive with
// [Archive.WriteFile], [Archive.Remove], [Archive.SetEntryPoint] and
// [Archive.EditManifest], then write it with [Archive.Save]. Check an
// archive without opening it with [Validate].
//
// Errors carry a stable [Code], such as [ErrEntryPointMissing], that
// callers check with errors.Is or [CodeOf] rather than by reading the
// message.
//
// The package doesn't render Markdown; applications do that.
package mdzip
