package qmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

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
	Binary         string
	Collection     string
	Mode           Mode
	MaxCandidates  int
	Timeout        time.Duration
	RefreshTimeout time.Duration
	Replay         string
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
		"binary": true, "collection": true, "mode": true, "max_candidates": true,
		"timeout_ms": true, "refresh_timeout_ms": true, "replay": true,
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
	return fmt.Sprintf("mode=%s collection=%s limit=%d", s.Mode, s.Collection, s.MaxCandidates)
}
