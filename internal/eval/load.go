package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePackPath reports a pack naming a file outside its own directory.
// A pack is a portable artifact; a path escaping it would make the pack
// depend on where it happens to be unpacked.
var ErrUnsafePackPath = errors.New("pack path escapes the pack directory")

// maxLine bounds one JSONL record. Cases and judgments are small by
// construction — a judgment names a lineage root, never a body — so a line
// past this is a malformed file, not a big one.
const maxLine = 1 << 20

// LoadPack reads and schema-checks the manifest at dir/pack.json.
//
// It does not read cases or judgments. Loading is three calls rather than one
// so that a runner can obtain the queries without ever holding the answers.
func LoadPack(dir string) (*Pack, error) {
	path := filepath.Join(dir, PackFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := ValidateBytes(KindPack, raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var p Pack
	if err := strictUnmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.dir = dir

	declared := []struct{ name, rel string }{
		{"cases", p.Cases},
		{"judgments", p.Judgments},
		{"sources", p.Sources},
		{"transcripts", p.Transcripts},
	}
	for _, d := range declared {
		if d.rel == "" {
			continue
		}
		if _, err := p.resolve(d.rel); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, d.name, err)
		}
	}
	return &p, nil
}

// resolve turns a pack-relative path into a real one, refusing anything that
// leaves the pack directory.
func (p *Pack) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafePackPath, rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePackPath, rel)
	}
	return filepath.Join(p.dir, clean), nil
}

// LoadCases reads the pack's case file.
//
// It never opens the judgment file. A runner may hand the result of this call
// straight to the agent under evaluation: there is nothing in it to leak.
func (p *Pack) LoadCases() ([]Case, error) {
	path, err := p.resolve(p.Cases)
	if err != nil {
		return nil, err
	}
	return loadJSONL[Case](path, KindCase)
}

// LoadJudgments reads the pack's judgment file: the answers, withheld from the
// agent under evaluation and read only to score what it returned.
func (p *Pack) LoadJudgments() ([]Judgment, error) {
	path, err := p.resolve(p.Judgments)
	if err != nil {
		return nil, err
	}
	return loadJSONL[Judgment](path, KindJudgment)
}

// loadJSONL reads one record per non-blank line, schema-checking each before
// decoding it. Errors name the file and line, because a pack is edited by
// hand and "invalid document" without a line number is not actionable.
func loadJSONL[T any](path string, kind Kind) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := ValidateBytes(kind, raw); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		var rec T
		if err := strictUnmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// strictUnmarshal refuses fields the Go type does not have. The schema already
// forbids unknown properties, so a rejection here means the schema and the
// type have drifted apart — which is exactly the failure worth catching.
func strictUnmarshal(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	return nil
}
