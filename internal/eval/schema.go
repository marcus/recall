package eval

import (
	"bytes"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/marcus/recall/eval/schema"
)

// Kind names one evaluation artifact schema.
type Kind string

const (
	KindPack     Kind = "pack"
	KindCase     Kind = "case"
	KindJudgment Kind = "judgment"
	KindRun      Kind = "run"
)

// Kinds lists every artifact schema. A schema file added to eval/schema
// without a Kind here is unreachable, and TestEverySchemaHasAKind says so.
var Kinds = []Kind{KindPack, KindCase, KindJudgment, KindRun}

func (k Kind) file() string { return string(k) + ".schema.json" }

// compiled is built once. Compilation reads only embedded bytes, so it cannot
// fail at runtime for any reason a test would not already have caught; the
// error is still returned rather than panicked on so a corrupt build reports
// itself instead of crashing a run.
var compiled = sync.OnceValues(compileSchemas)

func compileSchemas() (map[Kind]*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Formats are assertions here, not annotations: a case whose as_of is not a
	// real timestamp is a broken case, and finding that out at load time is the
	// whole point of validating.
	c.AssertFormat()

	ids := make(map[Kind]string, len(Kinds))
	for _, kind := range Kinds {
		raw, err := schema.FS.ReadFile(kind.file())
		if err != nil {
			return nil, fmt.Errorf("read %s schema: %w", kind, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse %s schema: %w", kind, err)
		}
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s schema is not an object", kind)
		}
		id, ok := obj["$id"].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("%s schema declares no $id", kind)
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("add %s schema: %w", kind, err)
		}
		ids[kind] = id
	}

	out := make(map[Kind]*jsonschema.Schema, len(ids))
	for kind, id := range ids {
		sch, err := c.Compile(id)
		if err != nil {
			return nil, fmt.Errorf("compile %s schema: %w", kind, err)
		}
		out[kind] = sch
	}
	return out, nil
}

// SchemaFiles lists the embedded schema file names.
func SchemaFiles() ([]string, error) {
	entries, err := fs.ReadDir(schema.FS, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// ValidateDocument checks one decoded JSON document against an artifact
// schema. doc must come from [jsonschema.UnmarshalJSON] or an equivalent
// generic decode; a Go struct is not a JSON document.
func ValidateDocument(kind Kind, doc any) error {
	schemas, err := compiled()
	if err != nil {
		return err
	}
	sch, ok := schemas[kind]
	if !ok {
		return fmt.Errorf("no schema for kind %q", kind)
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("%s schema: %w", kind, err)
	}
	return nil
}

// ValidateBytes decodes and checks one JSON document against an artifact
// schema.
func ValidateBytes(kind Kind, raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	return ValidateDocument(kind, doc)
}
