package mdzip

import (
	"errors"
	"io/fs"
	"strconv"
)

// Code is a stable error or warning code. The ERR_ codes in the first group
// are the ones the MDZip specification defines (Section 16.5); the rest are
// specific to this library.
//
// A Code is itself an error, so callers check for one with [errors.Is]:
//
//	if errors.Is(err, mdzip.ErrEntryPointMissing) { ... }
//
// or read it with [CodeOf]. Neither needs the message text.
type Code string

// Error returns the code itself, for example "ERR_ZIP_INVALID".
func (c Code) Error() string { return string(c) }

// Error codes from the MDZip specification, Section 16.5.
const (
	// ErrZipInvalid: not a valid ZIP archive, or an entry uses a compression
	// method other than 0 (stored) or 8 (deflate).
	ErrZipInvalid Code = "ERR_ZIP_INVALID"
	// ErrZipEncrypted: the archive or an entry is encrypted.
	ErrZipEncrypted Code = "ERR_ZIP_ENCRYPTED"
	// ErrPathInvalid: an entry path breaks Section 5.4. [Error.Detail] may
	// carry one of the more specific ErrPath... codes below.
	ErrPathInvalid Code = "ERR_PATH_INVALID"
	// ErrManifestInvalid: manifest.json isn't UTF-8 JSON, its top-level value
	// isn't an object, or a critical field has the wrong type or value.
	ErrManifestInvalid Code = "ERR_MANIFEST_INVALID"
	// ErrEntryPointUnresolved: no unambiguous primary Markdown file.
	ErrEntryPointUnresolved Code = "ERR_ENTRYPOINT_UNRESOLVED"
	// ErrEntryPointInvalid: manifest entryPoint isn't a valid entry point path.
	ErrEntryPointInvalid Code = "ERR_ENTRYPOINT_INVALID"
	// ErrEntryPointMissing: entryPoint is valid but matches no file,
	// including a name that differs only in case.
	ErrEntryPointMissing Code = "ERR_ENTRYPOINT_MISSING"
	// ErrVersionUnsupported: spec.version has a major version above 1.
	ErrVersionUnsupported Code = "ERR_VERSION_UNSUPPORTED"
	// ErrPathConflict: a file's path is also a directory of another entry.
	ErrPathConflict Code = "ERR_PATH_CONFLICT"
	// ErrDuplicatePath: two entries are equal after NFC normalization.
	ErrDuplicatePath Code = "ERR_DUPLICATE_PATH"
	// ErrModeUnsupported: manifest mode isn't "document" or "project".
	ErrModeUnsupported Code = "ERR_MODE_UNSUPPORTED"
	// ErrLimitExceeded: a resource limit was exceeded (see [DefaultLimits]),
	// including an entry that decompresses to more than its declared size.
	ErrLimitExceeded Code = "ERR_LIMIT_EXCEEDED"
)

// Detail codes for [ErrPathInvalid], shared with the other MDZip cores and
// used by the specification's path validation fixtures.
const (
	ErrPathEmpty        Code = "ERR_PATH_EMPTY"
	ErrPathLeadingSlash Code = "ERR_PATH_LEADING_SLASH"
	ErrPathTraversal    Code = "ERR_PATH_TRAVERSAL"
	ErrPathReservedChar Code = "ERR_PATH_RESERVED_CHAR"
	ErrPathControlChar  Code = "ERR_PATH_CONTROL_CHAR"
)

// Library-specific error codes.
const (
	// ErrEntryPointChanged: a save would change which file the entry point
	// resolves to, or leave it unresolvable (Section 14.1). Call
	// [Archive.SetEntryPoint] to change it on purpose.
	ErrEntryPointChanged Code = "ERR_ENTRYPOINT_CHANGED"
	// ErrNotFound: no file entry has that path. errors.Is(err, fs.ErrNotExist)
	// is also true.
	ErrNotFound Code = "ERR_NOT_FOUND"
	// ErrTextInvalid: an entry read as text isn't valid UTF-8.
	ErrTextInvalid Code = "ERR_TEXT_INVALID"
	// ErrIO: reading or writing a file on disk failed; see [errors.Unwrap].
	ErrIO Code = "ERR_IO"
)

// Warning codes, reported in [Finding]s. They never stop an archive opening.
const (
	// WarnFieldIgnored: a descriptive manifest field has the wrong type or an
	// invalid value and is ignored (Section 6.4). Path is the field, such as
	// "title" or "created.by".
	WarnFieldIgnored Code = "WARN_FIELD_IGNORED"
	// WarnDuplicateMember: manifest.json repeats a member name; the last one
	// is used (Section 6.4). Path is a JSON Pointer to the member.
	WarnDuplicateMember Code = "WARN_DUPLICATE_MEMBER"
	// WarnSpecialEntry: an entry is a symbolic link or other special file. It
	// is never followed and is not a file for reading or links (Section 13.1).
	// Saving keeps it unchanged; remove it with [Archive.Remove] to repair the
	// archive.
	WarnSpecialEntry Code = "WARN_SPECIAL_ENTRY"
	// WarnCaseCollision: two entries differ only in case, so they collide when
	// extracted to a case-insensitive filesystem (Section 5.4).
	WarnCaseCollision Code = "WARN_CASE_COLLISION"
	// WarnCoverMissing: manifest cover names no file in the archive.
	WarnCoverMissing Code = "WARN_COVER_MISSING"
	// WarnVersionOlder: spec.version has a major version below 1 (Section 11.2).
	WarnVersionOlder Code = "WARN_VERSION_OLDER"
)

// Error is the error type returned by this package.
type Error struct {
	// Code is the stable code, for example ErrEntryPointMissing.
	Code Code
	// Detail is a more specific code, or "". For ErrPathInvalid it says what
	// is wrong with the path, for example ErrPathTraversal.
	Detail Code
	// Path is the entry path or manifest field concerned, or "".
	Path string
	// Message describes the problem for people. Don't parse it.
	Message string
	// Err is the underlying error, if any.
	Err error
}

func (e *Error) Error() string {
	s := "mdzip: " + string(e.Code)
	if e.Detail != "" {
		s += " (" + string(e.Detail) + ")"
	}
	if e.Path != "" {
		s += ": " + strconv.Quote(e.Path)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is e's Code or Detail, so that
// errors.Is(err, mdzip.ErrPathTraversal) works. An ErrNotFound error also
// matches fs.ErrNotExist.
func (e *Error) Is(target error) bool {
	if c, ok := target.(Code); ok {
		return c == e.Code || (e.Detail != "" && c == e.Detail)
	}
	return target == fs.ErrNotExist && e.Code == ErrNotFound
}

// CodeOf returns the Code of err, or "" if err doesn't come from this package.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var c Code
	if errors.As(err, &c) {
		return c
	}
	return ""
}

func newError(code Code, path, msg string) *Error {
	return &Error{Code: code, Path: path, Message: msg}
}
