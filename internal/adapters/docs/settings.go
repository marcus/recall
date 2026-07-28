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

	// ExcludeDirs are glob patterns matched against a directory's own name.
	// A directory that matches is not walked at all.
	//
	// This is declared configuration rather than a rule compiled into the walk
	// because it decides what the corpus IS, exactly as Extensions does. An
	// exclusion nobody can see is a source answering "nothing found, coverage
	// complete" over content inside the root the operator named.
	ExcludeDirs []string

	// ExcludeNestedRepos keeps the walk out of a directory that holds a .git
	// entry of its own.
	//
	// A nested checkout is the failure the dot-class exclusion was really
	// aimed at: `.claude/worktrees/agent-1/` is a whole second copy of the
	// corpus, so the same document is indexed twice under distinct paths.
	// Lineage groups on source_record_id, those copies carry different ones,
	// and one document arrives as several independent lineage roots that
	// corroborate each other. Nothing downstream can detect it, because by
	// then the copies genuinely are distinct records.
	ExcludeNestedRepos bool

	// ExamplesQuoteQueries declares that this corpus quotes realistic user
	// queries as worked examples.
	//
	// Documentation does this deliberately and should: the relevance section of
	// docs/adapter-protocol.md is easier to read because it argues over "make a
	// dentist appointment" rather than over foo and bar. The cost is that a
	// corpus quoting user queries matches those queries, and half the answer to
	// `recall query dentist` was the retrieval system discussing the retrieval
	// of dentists.
	//
	// Where it is set, an occurrence inside a quotation does not count toward
	// how much a chunk is ABOUT the query. It stays indexed and stays findable:
	// somebody asking how relevance is defined must still reach that section,
	// and a chunk withheld for a low relevance is reported as a suppression
	// rather than dropped. What changes is that the chunk stops competing, on a
	// query about the thing it was quoting, with records that are that thing.
	//
	// It is declared per source rather than inferred because only the corpus
	// knows: a note quoting a decision IS the decision, and the same rule
	// applied there would discount the strongest evidence a personal corpus
	// holds.
	ExamplesQuoteQueries bool

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

// defaultExcludeDirs is the dot-class plus node_modules.
//
// The dot-class is a default rather than a law because the claim that
// `.github/` is the only dot-directory people write prose in does not survive
// contact with a real machine: .book/, .plans/, .kiro/, .agents/ and
// .specstory/ all hold prose in dot-directories today. A default that is wrong
// for one corpus costs that corpus an `exclude_dirs` line; a rule that is wrong
// for one corpus costs it its content with nothing to say so.
func defaultExcludeDirs() []string { return []string{".*", "node_modules"} }

// DefaultSettings is the configuration an instance gets when its settings block
// is empty.
func DefaultSettings() Settings {
	return Settings{
		Extensions:         defaultExtensions(),
		MaxFileBytes:       defaultMaxFileBytes,
		ExcludeDirs:        defaultExcludeDirs(),
		ExcludeNestedRepos: true,
	}
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

// excludedDir reports the declared pattern that keeps a directory out of the
// corpus, and whether one matched.
//
// Patterns match a directory's own NAME, not its path. Tool state appears at
// every depth of a corpus, so an exclusion written as a path would have to be
// restated for each place the same directory turns up, and the one place it was
// forgotten would be the one that got indexed.
func (s Settings) excludedDir(name string) (string, bool) {
	for _, pattern := range s.ExcludeDirs {
		// parseSettings rejected any pattern path.Match cannot compile, so a
		// non-nil error here can only mean a Settings value built in code.
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return pattern, true
		}
	}
	return "", false
}

// digest identifies the configuration a generation was built under. Root is
// excluded: it is recorded in the generation header, and a corpus moved to a
// new path is the same corpus.
//
// Every field that decides which files are in the corpus belongs here. The
// exclusions are two of them: a generation built while `.github/` was excluded
// describes a different corpus than one built after it was admitted, and an
// index that reported itself current across that change would be answering over
// a boundary nobody configured.
func (s Settings) digest() string {
	body, err := json.Marshal(struct {
		Extensions         []string            `json:"extensions"`
		MaxFileBytes       int64               `json:"max_file_bytes"`
		ExcludeDirs        []string            `json:"exclude_dirs"`
		ExcludeNestedRepos bool                `json:"exclude_nested_repos"`
		Aliases            map[string][]string `json:"aliases,omitempty"`
	}{s.Extensions, s.MaxFileBytes, s.ExcludeDirs, s.ExcludeNestedRepos, s.Aliases})
	// examples_quote_queries is deliberately absent: it changes how a query is
	// answered, not which files are in the corpus, so a generation built under
	// either value describes the same boundary and must not be rebuilt when it
	// is toggled.
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
		case "exclude_dirs":
			var list []string
			if list, err = asStrings(key, value); err == nil {
				err = out.setExcludeDirs(list)
			}
		case "exclude_nested_repos":
			out.ExcludeNestedRepos, err = asBool(key, value)
		case "examples_quote_queries":
			out.ExamplesQuoteQueries, err = asBool(key, value)
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

// setExcludeDirs validates the exclusion patterns.
//
// An empty list is accepted and means what it says: exclude nothing. That is
// the difference between a setting and a rule — a corpus whose author decided
// tool state is content must be able to say so, and today the only way to index
// a dot-directory is to point the root at it and hope nothing below it is a
// second checkout.
func (s *Settings) setExcludeDirs(list []string) error {
	out := make([]string, 0, len(list))
	for _, pattern := range list {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return fmt.Errorf("settings.exclude_dirs: want a directory name or glob, got an empty string")
		}
		if strings.ContainsRune(pattern, '/') {
			// A pattern is matched against one directory NAME. Accepting a path
			// would silently never match, which is an exclusion that looks
			// configured and excludes nothing.
			return fmt.Errorf("settings.exclude_dirs: want a directory name or glob like %q, got the path %q",
				"node_modules", pattern)
		}
		if _, err := path.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("settings.exclude_dirs: %q is not a valid glob: %w", pattern, err)
		}
		out = append(out, pattern)
	}
	sort.Strings(out)
	s.ExcludeDirs = out
	return nil
}

func asString(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("settings.%s: want a string, got %T", key, v)
	}
	return s, nil
}

func asBool(key string, v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("settings.%s: want true or false, got %T", key, v)
	}
	return b, nil
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
			"exclude_dirs": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "pattern": `^[^/]+$`},
				"description": "Directory-name globs never walked. Defaults to " +
					`[".*", "node_modules"]; an empty list excludes nothing.`,
			},
			"exclude_nested_repos": map[string]any{
				"type": "boolean",
				"description": "Skip any directory holding a .git entry, so a nested checkout " +
					"is not indexed as a second copy of the same documents. Default true.",
			},
			"examples_quote_queries": map[string]any{
				"type": "boolean",
				"description": "This corpus quotes realistic user queries as worked examples. " +
					"Where set, an occurrence inside a quotation does not count toward how much " +
					"a chunk is about the query; it stays indexed and findable. Default false.",
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
