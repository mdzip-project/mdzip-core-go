package mdzip

import (
	"os"
)

// Severity says whether a finding makes an archive non-conforming.
type Severity int

const (
	// SeverityError: the archive breaks a rule of the specification.
	SeverityError Severity = iota
	// SeverityWarning: allowed, but worth telling the user about.
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityError {
		return "error"
	}
	return "warning"
}

// Finding is one problem found while checking an archive.
type Finding struct {
	Severity Severity
	// Code is a stable code: an ERR_ code for errors, a WARN_ code for
	// warnings.
	Code Code
	// Detail is a more specific code, or "" (see [Error.Detail]).
	Detail Code
	// Path is the entry path or manifest field concerned, or "".
	Path string
	// Message describes the problem for people. Don't parse it.
	Message string

	err *Error
}

// Report is the result of [Validate].
type Report struct {
	Findings []Finding
}

// Valid reports whether the archive has no errors. Warnings don't count.
func (r *Report) Valid() bool { return len(r.Errors()) == 0 }

// Errors returns the findings with SeverityError.
func (r *Report) Errors() []Finding { return r.filter(SeverityError) }

// Warnings returns the findings with SeverityWarning.
func (r *Report) Warnings() []Finding { return r.filter(SeverityWarning) }

func (r *Report) filter(s Severity) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f)
		}
	}
	return out
}

// Validate checks an archive against the specification and returns every
// finding instead of stopping at the first, as [OpenBytes] does. On top of
// what OpenBytes checks, it reports an entry point that is missing or can't
// be determined as an error.
//
// Some checks depend on others: when the container or entry names are
// broken, the manifest and entry point aren't checked.
func Validate(data []byte) *Report {
	s := scan(data)
	r := &Report{Findings: s.findings}
	if s.hasErrors() {
		return r
	}
	a := &Archive{entries: s.entries, manifest: s.manifest}
	if _, err := a.EntryPoint(); err != nil {
		e := err.(*Error)
		r.Findings = append(r.Findings, Finding{Severity: SeverityError, Code: e.Code, Detail: e.Detail,
			Path: e.Path, Message: e.Message, err: e})
	}
	return r
}

// ValidateFile validates the archive at path. The error is only for failing
// to read the file (ErrIO).
func ValidateFile(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ErrIO, Path: path, Message: "can't read the file", Err: err}
	}
	return Validate(data), nil
}
