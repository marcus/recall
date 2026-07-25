package config

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"unicode"

	"github.com/marcus/recall/internal/recall"
)

// A source_uid is generated once and then lives forever inside persisted
// locators, evaluation judgments, and telemetry. The syntax is therefore
// deliberately dull: it must survive being pasted into a filename, a JSON key,
// and a locator's persisted form, where ":" is structural.
const (
	minUIDLen = 4
	maxUIDLen = 64
)

// crockford is Crockford base32: no padding, and no characters that a person
// can transcribe into a different one.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// GenerateSourceUID mints an identity for a new source instance. It is called
// once, when a source is added; nothing regenerates one, because every
// persisted reference already keys on the old value.
func GenerateSourceUID() (recall.SourceUID, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating source_uid: %w", err)
	}
	return recall.SourceUID(crockford.EncodeToString(b[:])), nil
}

// validateSourceUID checks the syntax of an immutable identity.
func validateSourceUID(uid recall.SourceUID) error {
	s := string(uid)
	switch {
	case s == "":
		return fmt.Errorf("source_uid is required and is generated once, never edited")
	case len(s) < minUIDLen || len(s) > maxUIDLen:
		return fmt.Errorf("source_uid %q must be %d to %d characters", s, minUIDLen, maxUIDLen)
	}
	for _, r := range s {
		if !isIdentRune(r) {
			return fmt.Errorf("source_uid %q may contain only letters, digits, %q, and %q",
				s, "-", "_")
		}
	}
	return nil
}

// validateSourceID checks a display name.
//
// The one structural rule is the colon: locator text is "<source_id>:<local>"
// and only the first colon is structural, so a name containing one would make
// every locator it prints ambiguous.
func validateSourceID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("source_id is required")
	case strings.Contains(id, ":"):
		return fmt.Errorf("source_id %q may not contain %q: it is the locator separator", id, ":")
	case len(id) > maxUIDLen:
		return fmt.Errorf("source_id %q must be at most %d characters", id, maxUIDLen)
	}
	for _, r := range id {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("source_id %q may not contain whitespace or control characters", id)
		}
	}
	return nil
}

// validateName checks an adapter or profile name. A profile name becomes a
// path element under the state and cache directories, so a separator or a
// traversal segment would let it escape.
func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s name %q is not usable as a directory name", kind, name)
	}
	for _, r := range name {
		if !isIdentRune(r) && r != '.' {
			return fmt.Errorf("%s name %q may contain only letters, digits, %q, %q, and %q",
				kind, name, "-", "_", ".")
		}
	}
	return nil
}

func isIdentRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return r == '-' || r == '_'
	}
}

// validateEnvVarName checks a referenced environment variable name. A name
// with an "=" or a NUL could never be read back, and a name with whitespace is
// almost always a pasted value rather than a reference.
func validateEnvVarName(name string) error {
	if name == "" {
		return fmt.Errorf("env_var must name an environment variable")
	}
	for _, r := range name {
		if r == '=' || r == 0 || unicode.IsSpace(r) {
			return fmt.Errorf("env_var %q is not a usable variable name", name)
		}
	}
	return nil
}
