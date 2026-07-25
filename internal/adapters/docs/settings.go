package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Settings is the adapter-owned settings block for one document source
// instance. Every field here changes an explained code path; a field that
// configured nothing would be a defect.
type Settings struct {
	// Root is the corpus root. Empty means the instance's configured location,
	// which is the normal case; Root exists for an instance whose location
	// names something broader than the directory to index.
	Root string

	// Extensions are the file suffixes that count as documents.
	Extensions []string

	// MaxFileBytes rejects a file too large to be a document. It is a record
	// failure, not a build failure: one pathological file must not cost the
	// corpus its index.
	MaxFileBytes int64

	// Aliases are declared stable names for a document, keyed by its
	// corpus-relative path. They are the only single-word identifiers that may
	// produce an exact_identifier signal, because a person wrote them down for
	// this corpus.
	Aliases map[string][]string
}

// Defaults for a Markdown corpus.
const (
	defaultMaxFileBytes int64 = 1 << 20
)

func defaultExtensions() []string { return []string{".markdown", ".md"} }

// DefaultSettings is the configuration an instance gets when its settings block
// is empty.
func DefaultSettings() Settings {
	return Settings{Extensions: defaultExtensions(), MaxFileBytes: defaultMaxFileBytes}
}

// indexes reports whether a corpus-relative path is a document this instance
// indexes.
func (s Settings) indexes(rel string) bool {
	if strings.Contains(rel, "#") {
		// '#' separates a locator's path from its line range. A path containing
		// one could not be printed as a locator and read back, so it is not
		// indexable at all.
		return false
	}
	ext := strings.ToLower(path.Ext(rel))
	for _, want := range s.Extensions {
		if ext == want {
			return true
		}
	}
	return false
}

// digest identifies the configuration a generation was built under. Root is
// excluded: it is recorded in the generation header, and a corpus moved to a
// new path is the same corpus.
func (s Settings) digest() string {
	body, err := json.Marshal(struct {
		Extensions   []string            `json:"extensions"`
		MaxFileBytes int64               `json:"max_file_bytes"`
		Aliases      map[string][]string `json:"aliases,omitempty"`
	}{s.Extensions, s.MaxFileBytes, s.Aliases})
	if err != nil {
		// Every field is a string, an int, or a map of them. A failure here
		// would mean the type above changed into something unserializable.
		panic("docs: settings digest: " + err.Error())
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:digestLength]
}

// parseSettings validates the instance's settings block.
//
// The manifest's settings_schema reaches the core only in the initialize
// result, so on a first handshake the adapter is the only thing that can check
// its own settings. It fails the handshake with a readable error rather than
// starting with a silently ignored key.
func parseSettings(raw map[string]any) (Settings, error) {
	out := DefaultSettings()
	for _, key := range sortedKeys(raw) {
		value := raw[key]
		var err error
		switch key {
		case "root":
			out.Root, err = asString(key, value)
		case "extensions":
			var list []string
			if list, err = asStrings(key, value); err == nil {
				err = out.setExtensions(list)
			}
		case "max_file_bytes":
			var n float64
			if n, err = asNumber(key, value); err == nil {
				if n < 1 {
					err = fmt.Errorf("settings.max_file_bytes: want a positive size, got %v", n)
				}
				out.MaxFileBytes = int64(n)
			}
		case "aliases":
			out.Aliases, err = asAliases(value)
		default:
			err = fmt.Errorf("settings.%s: unknown setting", key)
		}
		if err != nil {
			return Settings{}, err
		}
	}
	return out, nil
}

func (s *Settings) setExtensions(list []string) error {
	if len(list) == 0 {
		return fmt.Errorf("settings.extensions: want at least one suffix")
	}
	out := make([]string, 0, len(list))
	for _, ext := range list {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if !strings.HasPrefix(ext, ".") || len(ext) < 2 {
			return fmt.Errorf("settings.extensions: want a suffix like %q, got %q", ".md", ext)
		}
		out = append(out, ext)
	}
	sort.Strings(out)
	s.Extensions = out
	return nil
}

func asString(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("settings.%s: want a string, got %T", key, v)
	}
	return s, nil
}

func asNumber(key string, v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	case int:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("settings.%s: want a number, got %T", key, v)
	}
}

func asStrings(key string, v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		if list, ok := v.([]string); ok {
			return list, nil
		}
		return nil, fmt.Errorf("settings.%s: want an array of strings, got %T", key, v)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("settings.%s[%d]: want a string, got %T", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}

func asAliases(v any) (map[string][]string, error) {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("settings.aliases: want an object of path to names, got %T", v)
	}
	out := make(map[string][]string, len(raw))
	for _, p := range sortedKeys(raw) {
		names, err := asStrings("aliases."+p, raw[p])
		if err != nil {
			return nil, err
		}
		clean := make([]string, 0, len(names))
		for _, name := range names {
			if len(tokenize(name)) == 0 {
				return nil, fmt.Errorf("settings.aliases[%q]: %q has no searchable text", p, name)
			}
			clean = append(clean, name)
		}
		sort.Strings(clean)
		out[path.Clean(p)] = clean
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// settingsSchema is the manifest's declared shape for the block above. The core
// validates against it on every later handshake and `recall doctor` uses it to
// check a configuration without starting a query.
func settingsSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"root": map[string]any{
				"type":        "string",
				"description": "Corpus root. Defaults to the instance's configured location.",
			},
			"extensions": map[string]any{
				"type":        "array",
				"minItems":    1,
				"items":       map[string]any{"type": "string", "pattern": `^\.[A-Za-z0-9]+$`},
				"description": "File suffixes indexed as documents.",
			},
			"max_file_bytes": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Files larger than this are counted as failures, not indexed.",
			},
			"aliases": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"description": "Declared stable names per corpus-relative path. Only these may match as exact identifiers on a single token.",
			},
		},
	}
}
