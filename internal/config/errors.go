package config

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel classes. Callers match on these; `recall doctor` renders them.
var (
	// ErrTrustBoundary means an untrusted project file tried to do something
	// only user configuration may do. It is always fatal: a warning here would
	// mean a cloned repository got to influence what Recall executes.
	ErrTrustBoundary = errors.New("trust boundary violation")

	// ErrInvalid means the configuration is internally inconsistent or out of
	// range. Loading fails; no partial configuration is returned, because a
	// half-applied configuration would silently change which sources answer.
	ErrInvalid = errors.New("invalid configuration")
)

// Error is one problem, located. Both the file and the offending key are
// carried separately so a machine-readable surface can report them without
// parsing the message.
type Error struct {
	// File is the configuration file the problem was found in. It is empty
	// only for problems that belong to no single file, such as a duplicate
	// spanning two of them, which name both files in Msg instead.
	File string `json:"file,omitempty"`
	// Key is the dotted TOML path, with array indices, for example
	// "sources[1].base_prior".
	Key string `json:"key,omitempty"`
	Msg string `json:"message"`

	// class is ErrTrustBoundary or ErrInvalid.
	class error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.File != "" {
		fmt.Fprintf(&b, "%s: ", e.File)
	}
	if e.Key != "" {
		fmt.Fprintf(&b, "key %q: ", e.Key)
	}
	b.WriteString(e.Msg)
	return b.String()
}

func (e *Error) Unwrap() error { return e.class }

// trustErrorf reports a project file reaching past the trust boundary.
func trustErrorf(file, key, format string, args ...any) *Error {
	return &Error{File: file, Key: key, Msg: fmt.Sprintf(format, args...), class: ErrTrustBoundary}
}

// invalidErrorf reports a configuration that cannot be used as written.
func invalidErrorf(file, key, format string, args ...any) *Error {
	return &Error{File: file, Key: key, Msg: fmt.Sprintf(format, args...), class: ErrInvalid}
}

// problems accumulates validation errors so one load reports everything wrong
// rather than one thing at a time. Trust boundary violations do not go here:
// they abort immediately, before anything else is decoded.
type problems []error

func (p *problems) add(err error) {
	if err != nil {
		*p = append(*p, err)
	}
}

func (p *problems) err() error {
	if len(*p) == 0 {
		return nil
	}
	return errors.Join(*p...)
}
