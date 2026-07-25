package protocol

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/*.json
var schemaFS embed.FS

// schemaBase is the identifier space the embedded schemas declare. Nothing is
// ever fetched from it: every document is registered before compilation, and
// the compiler has no loader for anything else.
const schemaBase = "https://recall.dev/schema/v1/"

// SchemaSet is the compiled contract for every payload that crosses the wire.
//
// Both ends hold one. The sender validates before writing and the receiver
// validates after reading, so a shape that violates the contract is reported by
// the end that produced it instead of turning into a confusing decode failure
// three layers later.
type SchemaSet struct {
	byName map[string]*jsonschema.Schema
}

// payloadNames maps a method to the schema for its params and its result. A
// method absent here has no wire contract and is rejected.
var payloadNames = map[string]struct{ params, result string }{
	MethodInitialize: {"initialize_params", "manifest"},
	MethodSearch:     {"search_params", "search_result"},
	MethodExpand:     {"expand_params", "expand_result"},
	MethodHealth:     {"health_params", "health_result"},
	MethodRefresh:    {"refresh_params", "health_result"},
	MethodCancel:     {"cancel_params", ""},
	MethodShutdown:   {"empty", "empty"},
}

var loadSchemas = sync.OnceValues(compileSchemas)

// Schemas returns the compiled schema set. Compilation happens once per
// process; the result is immutable and safe to share.
func Schemas() (*SchemaSet, error) { return loadSchemas() }

func compileSchemas() (*SchemaSet, error) {
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, fmt.Errorf("read embedded schemas: %w", err)
	}

	c := jsonschema.NewCompiler()
	// Formats are assertions here, not annotations: a timestamp that is not an
	// instant is a contract break, and catching it at the boundary is the point
	// of validating at all.
	c.AssertFormat()

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := schemaFS.ReadFile(path.Join("schemas", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if err := c.AddResource(schemaBase+e.Name(), doc); err != nil {
			return nil, fmt.Errorf("add %s: %w", e.Name(), err)
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)

	set := &SchemaSet{byName: make(map[string]*jsonschema.Schema, len(names))}
	for _, name := range names {
		sch, err := c.Compile(schemaBase + name + ".json")
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", name, err)
		}
		set.byName[name] = sch
	}

	// Every method's declared schema must exist, or a payload would silently go
	// unchecked. Failing at compile time makes that impossible to ship.
	for method, p := range payloadNames {
		for _, name := range []string{p.params, p.result} {
			if name == "" {
				continue
			}
			if _, ok := set.byName[name]; !ok {
				return nil, fmt.Errorf("method %s names missing schema %q", method, name)
			}
		}
	}
	return set, nil
}

// Names lists the compiled schemas, in order.
func (s *SchemaSet) Names() []string {
	out := make([]string, 0, len(s.byName))
	for name := range s.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ValidateParams checks a request's params against its method's schema.
func (s *SchemaSet) ValidateParams(method string, raw json.RawMessage) error {
	p, ok := payloadNames[method]
	if !ok {
		return Errorf(CodeMethodNotFound, "unknown method %q", method)
	}
	if err := s.validate(p.params, raw); err != nil {
		return Errorf(CodeInvalidParams, "%s params: %s", method, err)
	}
	return nil
}

// ValidateResult checks a response's result against its method's schema. A
// method with no result contract, such as the cancel notification, has nothing
// to check and reports as much.
func (s *SchemaSet) ValidateResult(method string, raw json.RawMessage) error {
	p, ok := payloadNames[method]
	if !ok {
		return Errorf(CodeMethodNotFound, "unknown method %q", method)
	}
	if p.result == "" {
		return Errorf(CodeInvalidRequest, "%s returns no result", method)
	}
	if err := s.validate(p.result, raw); err != nil {
		return Errorf(CodeInternal, "%s result: %s", method, err)
	}
	return nil
}

func (s *SchemaSet) validate(name string, raw json.RawMessage) error {
	sch, ok := s.byName[name]
	if !ok {
		return fmt.Errorf("no schema %q", name)
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	// jsonschema wants numbers as json.Number so large integers keep their
	// exact value through validation.
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return errors.New("payload is not valid JSON")
	}
	if err := sch.Validate(inst); err != nil {
		return errors.New(locations(err))
	}
	return nil
}

// maxReportedLocations bounds how much of a validation failure is reported. A
// message listing every field of a large candidate list helps nobody.
const maxReportedLocations = 5

// locations renders a validation failure as the places that failed, not the
// values that failed. Instance values may be source content, and diagnostics do
// not carry source content.
func locations(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return "does not match the schema"
	}
	var (
		seen  = map[string]bool{}
		parts []string
	)
	var walk func(u *jsonschema.OutputUnit)
	walk = func(u *jsonschema.OutputUnit) {
		if len(u.Errors) == 0 {
			at := u.InstanceLocation
			if at == "" {
				at = "/"
			}
			key := at + " " + u.KeywordLocation
			if !seen[key] && len(parts) < maxReportedLocations {
				seen[key] = true
				parts = append(parts, fmt.Sprintf("at %s (%s)", at, u.KeywordLocation))
			}
			return
		}
		for i := range u.Errors {
			walk(&u.Errors[i])
		}
	}
	walk(ve.BasicOutput())
	if len(parts) == 0 {
		return "does not match the schema"
	}
	return strings.Join(parts, "; ")
}
