package qmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
)

// Mode selects how many of qmd's retrieval layers a search uses.
//
// It exists so an evaluation can attribute a gain or a regression to a layer
// instead of to one opaque pipeline. qmd stacks LLM query expansion, RRF
// fusion over an FTS list and a vector list, and a cross-encoder rerank; a
// result that improved under all three at once tells nobody which of them
// earned its place. Each mode is recorded in `index_config`, so two runs under
// different modes can never be compared by accident.
type Mode string

const (
	// ModeBM25 is `qmd search`: SQLite FTS5 only, no model of any kind. It is
	// the sanity anchor against Recall's own lexical document adapter, and it
	// is the one mode that honestly returns an empty list for an off-corpus
	// query — the reranked modes score unrelated documents at the same value a
	// genuinely relevant one earns.
	ModeBM25 Mode = "bm25"

	// ModeVector is `qmd vsearch`: embedding similarity only.
	ModeVector Mode = "vector"

	// ModeHybrid is `qmd query --no-rerank`: RRF fusion over the FTS and
	// vector lists, with the reranker off.
	//
	// It is NOT expansion-free. qmd applies LLM query expansion to any
	// single-line query, and `--no-rerank` disables only the reranker, so the
	// layer this mode isolates is the reranker and nothing else. Attribution of
	// the expansion layer is by comparison with bm25 and vector, which run no
	// model over the query at all. See the note in doc.go.
	ModeHybrid Mode = "hybrid"

	// ModeFull is `qmd query`: expansion, fusion, and reranking.
	ModeFull Mode = "full"
)

// modes is the closed vocabulary, in the order documentation lists them.
var modes = []Mode{ModeBM25, ModeVector, ModeHybrid, ModeFull}

// Embeds reports whether this mode needs the embedding model and the vector
// half of the index. A mode that does is not merely slower without them: it
// cannot see the corpus at all, so coverage depends on this answer.
func (m Mode) Embeds() bool { return m != ModeBM25 }

// Expands reports whether qmd runs its LLM query-expansion model. It is the
// nondeterministic layer, which is why committed evaluation packs replay
// recorded output rather than spawning qmd.
func (m Mode) Expands() bool { return m == ModeHybrid || m == ModeFull }

// Reranks reports whether qmd runs the cross-encoder reranker.
func (m Mode) Reranks() bool { return m == ModeFull }

// Defaults for the settings block.
const (
	defaultBinary        = "qmd"
	defaultMode          = ModeHybrid
	defaultMaxCandidates = 25
	defaultTimeout       = 45 * time.Second
	// Refresh reindexes and then loads three GGUF models. On the corpus this
	// adapter was developed against `qmd embed` takes 14 seconds and a cold
	// model load takes tens more; a first refresh after an install downloads
	// about 2GB. The bound is generous because the alternative to waiting is a
	// half-built index.
	defaultRefreshTimeout = 15 * time.Minute
)

// Settings is the validated adapter-owned configuration block.
type Settings struct {
	Inference         *HTTPInference
	Binary            string
	Collection        string
	Mode              Mode
	MaxCandidates     int
	Timeout           time.Duration
	RefreshTimeout    time.Duration
	Replay            string
	DeriveExtensions  []string
	DerivedCollection string
	Selection         document.Selection
	MaxRawBytes       int64
	MaxTextBytes      int64
	derivedSettings   bool
}

func parseSettings(raw map[string]any, baseDir string) (Settings, error) {
	set := Settings{
		Binary:         defaultBinary,
		Mode:           defaultMode,
		MaxCandidates:  defaultMaxCandidates,
		Timeout:        defaultTimeout,
		RefreshTimeout: defaultRefreshTimeout,
	}
	allowed := map[string]bool{
		"inference_tokenizer_url": true, "inference_provider": true, "inference_url": true, "inference_model": true, "inference_dimensions": true, "inference_qmd_package": true, "inference_index": true, "inference_node": true, "inference_api_key_env": true, "inference_rook": true,
		"binary": true, "collection": true, "mode": true, "max_candidates": true,
		"timeout_ms": true, "refresh_timeout_ms": true, "replay": true,
		"derive_extensions": true, "derived_collection": true, "exclude_dirs": true, "exclude_globs": true, "sibling_preference": true, "exclude_nested_repos": true, "max_raw_bytes": true, "max_file_bytes": true,
	}
	for key := range raw {
		if !allowed[key] {
			// A misspelled setting that silently did nothing is the same defect
			// as an undeclared one: it will be set by someone who then believes
			// it took effect.
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"qmd: unknown setting %q", key)
		}
	}

	var err error
	if set.Binary, err = stringSetting(raw, "binary", set.Binary); err != nil {
		return Settings{}, err
	}
	if set.Binary = strings.TrimSpace(set.Binary); set.Binary == "" {
		set.Binary = defaultBinary
	}

	if set.Collection, err = stringSetting(raw, "collection", ""); err != nil {
		return Settings{}, err
	}
	set.Collection = strings.TrimSpace(set.Collection)
	if set.Collection == "" {
		// Required, and not defaulted from the location's base name. A guessed
		// collection name would search whichever corpus happened to carry that
		// name in the caller's qmd index, which is the wrong-corpus failure
		// this adapter verifies against on every operation.
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting \"collection\" is required; it names the qmd collection this source searches")
	}
	if !namePattern.MatchString(set.Collection) {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: collection %q is not a qmd collection name", sanitizeLine(set.Collection))
	}

	modeText, err := stringSetting(raw, "mode", string(defaultMode))
	if err != nil {
		return Settings{}, err
	}
	mode := Mode(strings.TrimSpace(strings.ToLower(modeText)))
	if mode == "" {
		mode = defaultMode
	}
	if !validMode(mode) {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting \"mode\" must be one of %s", modeList())
	}
	set.Mode = mode

	if set.MaxCandidates, err = intSetting(raw, "max_candidates", set.MaxCandidates, 1); err != nil {
		return Settings{}, err
	}
	if set.MaxCandidates > 999 {
		// The cap is qmd's `-n` argument, and the argv allowlist accepts at
		// most four digits for it. Refusing here names the limit instead of
		// failing later inside a spawn guard.
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting \"max_candidates\" must be at most 999")
	}

	timeoutMS, err := intSetting(raw, "timeout_ms", int(defaultTimeout/time.Millisecond), 1)
	if err != nil {
		return Settings{}, err
	}
	set.Timeout = time.Duration(timeoutMS) * time.Millisecond

	refreshMS, err := intSetting(raw, "refresh_timeout_ms", int(defaultRefreshTimeout/time.Millisecond), 1)
	if err != nil {
		return Settings{}, err
	}
	set.RefreshTimeout = time.Duration(refreshMS) * time.Millisecond

	if set.Replay, err = stringSetting(raw, "replay", ""); err != nil {
		return Settings{}, err
	}
	set.Replay = strings.TrimSpace(set.Replay)
	if set.Replay != "" && !filepath.IsAbs(set.Replay) {
		// Resolved against the directory of the file that declared the source,
		// never against the process working directory: the latter would make
		// one configuration read different fixtures depending on where Recall
		// was started.
		if baseDir == "" {
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"qmd: relative replay path requires base_dir")
		}
		set.Replay = filepath.Join(baseDir, set.Replay)
	}
	if set.Inference, err = parseInference(raw, set.Mode, set.Replay); err != nil {
		return Settings{}, err
	}
	if err := set.parseDerived(raw); err != nil {
		return Settings{}, err
	}
	return set, nil
}

func validMode(m Mode) bool {
	for _, got := range modes {
		if got == m {
			return true
		}
	}
	return false
}

func modeList() string {
	names := make([]string, 0, len(modes))
	for _, m := range modes {
		names = append(names, string(m))
	}
	return strings.Join(names, " | ")
}

func stringSetting(raw map[string]any, key, fallback string) (string, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	got, ok := value.(string)
	if !ok {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting %q must be a string", key)
	}
	return got, nil
}

func intSetting(raw map[string]any, key string, fallback, minimum int) (int, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	var got int
	switch n := value.(type) {
	case int:
		got = n
	case int64:
		got = int(n)
	case float64:
		if n != float64(int(n)) {
			return 0, protocol.Errorf(protocol.CodeInvalidParams,
				"qmd: setting %q must be an integer", key)
		}
		got = int(n)
	default:
		return 0, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting %q must be an integer", key)
	}
	if got < minimum {
		return 0, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: setting %q must be at least %d", key, minimum)
	}
	return got, nil
}

func settingsSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"collection"},
		"properties": map[string]any{
			"binary": map[string]any{
				"type":        "string",
				"description": "Path to the qmd executable. Empty means qmd on PATH.",
			},
			"collection": map[string]any{
				"type":        "string",
				"description": "Name of the qmd collection this source searches. Required, and verified against the source location on every operation: a collection indexing a different directory makes the source unavailable rather than answering from the wrong corpus.",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []any{string(ModeBM25), string(ModeVector), string(ModeHybrid), string(ModeFull)},
				"description": "Which qmd retrieval layers run: bm25 is FTS only and needs no model, vector is embeddings only, hybrid adds RRF fusion with the reranker off, full adds the reranker. The mode is part of index_config, so runs under different modes are never compared by accident.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 999,
				"description": "Maximum candidates one search asks qmd for. A request limit below it wins.",
			},
			"timeout_ms": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Maximum duration of one qmd invocation; the request deadline may shorten it. A reranked query on a warm model takes seconds, so a low value turns full mode into a timeout rather than a faster answer.",
			},
			"refresh_timeout_ms": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Maximum duration of one recall/refresh, which reindexes, embeds, and warms qmd's models. A first refresh after installing qmd downloads about 2GB of model files and needs minutes.",
			},
			"replay": map[string]any{
				"type":        "string",
				"description": "Directory of recorded qmd output used instead of spawning qmd, for conformance transcripts and committed evaluation packs; relative paths resolve from base_dir. A replaying source verifies no store and publishes no store identity.",
			},
		},
	}
}

// settingsDigest is the configuration this source's answers were produced
// under, for index_config. It names the retrieval configuration and nothing
// about where this instance found it.
func (s Settings) settingsDigest() string {
	digest := fmt.Sprintf("mode=%s collection=%s limit=%d", s.Mode, s.Collection, s.MaxCandidates)
	if s.Inference != nil {
		digest += " provider=" + s.Inference.identity() + " expansion=none"
	}
	if len(s.DeriveExtensions) > 0 {
		data, _ := json.Marshal([]any{s.DeriveExtensions, s.DerivedCollection, s.Selection, s.MaxRawBytes, s.MaxTextBytes, document.HTMLExtractionVersion})
		digest += fmt.Sprintf(" derived=%x", sha256.Sum256(data))
	}
	return digest
}

// Derived settings are opt-in. Keep the legacy manifest and digest byte-identical
// when unset, including recorded protocol handshakes.
func (s *Settings) parseDerived(raw map[string]any) error {
	s.Selection = document.Selection{ExcludeDirs: []string{".*", "node_modules"}, ExcludeNestedRepos: true}
	s.MaxRawBytes = document.DefaultMaxRawBytes
	s.MaxTextBytes = 1 << 20
	for _, key := range []string{"derive_extensions", "derived_collection", "exclude_dirs", "exclude_globs", "sibling_preference", "exclude_nested_repos", "max_raw_bytes", "max_file_bytes"} {
		if _, ok := raw[key]; ok {
			s.derivedSettings = true
		}
	}
	for key, dst := range map[string]*[]string{"derive_extensions": &s.DeriveExtensions, "exclude_dirs": &s.Selection.ExcludeDirs, "exclude_globs": &s.Selection.ExcludeGlobs, "sibling_preference": &s.Selection.SiblingPreference} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var list []string
		switch vv := v.(type) {
		case []string:
			list = append([]string{}, vv...)
		case []any:
			for _, x := range vv {
				t, ok := x.(string)
				if !ok {
					return protocol.Errorf(protocol.CodeInvalidParams, "qmd: %s must be a string list", key)
				}
				list = append(list, t)
			}
		default:
			return protocol.Errorf(protocol.CodeInvalidParams, "qmd: %s must be a string list", key)
		}
		*dst = list
	}
	for i, ext := range s.DeriveExtensions {
		ext = strings.ToLower(ext)
		if !document.IsHTML("file" + ext) {
			return protocol.Errorf(protocol.CodeInvalidParams, "qmd: derive_extensions supports .html and .htm")
		}
		s.DeriveExtensions[i] = ext
	}
	var err error
	if s.DerivedCollection, err = stringSetting(raw, "derived_collection", ""); err != nil {
		return err
	}
	if len(s.DeriveExtensions) > 0 && (!namePattern.MatchString(s.DerivedCollection) || s.DerivedCollection == s.Collection) {
		return protocol.Errorf(protocol.CodeInvalidParams, "qmd: derived_collection must be a distinct collection name when derive_extensions is set")
	}
	if v, ok := raw["exclude_nested_repos"]; ok {
		b, ok := v.(bool)
		if !ok {
			return protocol.Errorf(protocol.CodeInvalidParams, "qmd: exclude_nested_repos must be boolean")
		}
		s.Selection.ExcludeNestedRepos = b
	}
	for _, pattern := range s.Selection.ExcludeDirs {
		if pattern == "" || strings.ContainsAny(pattern, "/\\") {
			return protocol.Errorf(protocol.CodeInvalidParams, "qmd: exclude_dirs requires basename globs")
		}
		if _, err := path.Match(pattern, "probe"); err != nil {
			return protocol.Errorf(protocol.CodeInvalidParams, "qmd: invalid exclude_dirs glob")
		}
	}
	if err = s.Selection.Validate(); err != nil {
		return protocol.Errorf(protocol.CodeInvalidParams, "qmd: %s", err)
	}
	n, err := intSetting(raw, "max_raw_bytes", int(s.MaxRawBytes), 1)
	if err != nil {
		return err
	}
	s.MaxRawBytes = int64(n)
	n, err = intSetting(raw, "max_file_bytes", int(s.MaxTextBytes), 1)
	if err != nil {
		return err
	}
	s.MaxTextBytes = int64(n)
	return nil
}

func (s Settings) schema() map[string]any {
	schema := settingsSchema()
	props := schema["properties"].(map[string]any)
	if s.Inference != nil {
		for _, key := range []string{"inference_provider", "inference_url", "inference_tokenizer_url", "inference_model", "inference_qmd_package", "inference_index", "inference_node", "inference_api_key_env"} {
			props[key] = map[string]any{"type": "string"}
		}
		props["inference_dimensions"] = map[string]any{"type": "integer", "minimum": 1}
		props["inference_rook"] = map[string]any{"type": "boolean"}
	}
	if !s.derivedSettings {
		return schema
	}
	for _, key := range []string{"derive_extensions", "exclude_dirs", "exclude_globs", "sibling_preference"} {
		props[key] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}
	props["derived_collection"] = map[string]any{"type": "string", "description": "Collection over the refresh-built HTML Markdown mirror; required with derive_extensions."}
	props["exclude_nested_repos"] = map[string]any{"type": "boolean"}
	for _, key := range []string{"max_raw_bytes", "max_file_bytes"} {
		props[key] = map[string]any{"type": "integer", "minimum": 1}
	}
	return schema
}
