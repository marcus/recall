package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// rawFile is the TOML document shape, shared by both layers. Which parts a
// layer may use is a trust decision, not a syntax one, so both decode into the
// same type and the difference is enforced explicitly.
//
// Every scalar is a pointer so that "declared as false" and "not declared" stay
// distinguishable. The merge and [Config.Explain] both depend on that
// difference: a default must never be reported as something a user wrote.
type rawFile struct {
	Defaults *rawDefaults          `toml:"defaults"`
	Adapters map[string]rawAdapter `toml:"adapters"`
	Sources  []rawSource           `toml:"sources"`
	Profiles map[string]rawProfile `toml:"profiles"`
}

type rawDefaults struct {
	Profile         *string `toml:"profile"`
	TimeoutMS       *int    `toml:"timeout_ms"`
	FusionReserveMS *int    `toml:"fusion_reserve_ms"`
}

type rawAdapter struct {
	Command        *string              `toml:"command"`
	Args           []string             `toml:"args"`
	Env            map[string]string    `toml:"env"`
	Secrets        map[string]rawSecret `toml:"secrets"`
	FreshnessModes []string             `toml:"freshness_modes"`
	Conformance    *string              `toml:"conformance"`
}

type rawSecret struct {
	EnvVar   *string `toml:"env_var"`
	Keychain *string `toml:"keychain"`
}

type rawSource struct {
	SourceUID       *string              `toml:"source_uid"`
	SourceID        *string              `toml:"source_id"`
	Adapter         *string              `toml:"adapter"`
	Location        *string              `toml:"location"`
	LocationKind    *string              `toml:"location_kind"`
	Enabled         *bool                `toml:"enabled"`
	RecordTypes     []string             `toml:"record_types"`
	FreshnessMode   *string              `toml:"freshness_mode"`
	FreshnessPolicy *string              `toml:"freshness_policy"`
	BasePrior       *float64             `toml:"base_prior"`
	IntentPriors    map[string]float64   `toml:"intent_priors"`
	Sensitivity     *string              `toml:"sensitivity"`
	TimeoutMS       *int                 `toml:"timeout_ms"`
	Settings        map[string]any       `toml:"settings"`
	Secrets         map[string]rawSecret `toml:"secrets"`
}

type rawProfile struct {
	Sources        []string `toml:"sources"`
	MaxSensitivity *string  `toml:"max_sensitivity"`
}

// sourceFile is one parsed configuration file and everything needed to report
// where its values came from.
type sourceFile struct {
	Path   string
	Dir    string
	Layer  Layer
	Raw    rawFile
	Origin Origin
}

// executableKeys are the keys that can turn a configuration file into code
// execution. A project file naming any of them, anywhere in its document, is
// rejected.
//
// The list is by name rather than by position on purpose. A future key that
// merely looks executable is caught before it is implemented, and a key hidden
// inside an adapter-owned settings table — the one place a reviewer is least
// likely to look — is caught with the same force as a top-level one.
var executableKeys = map[string]string{
	"command":     "an adapter command",
	"args":        "adapter argv",
	"argv":        "adapter argv",
	"env":         "a subprocess environment",
	"environment": "a subprocess environment",
	"exec":        "an adapter command",
	"entrypoint":  "an adapter command",
	"shell":       "an adapter command",
}

// checkTrustBoundary rejects an untrusted document that reaches for anything
// executable. It runs over the raw TOML tree before typed decoding, so a key
// the typed shape does not even model still fails loudly rather than being
// dropped as unknown.
func checkTrustBoundary(path string, doc map[string]any) error {
	// The adapters table exists only to define what Recall runs. A project file
	// has nothing legitimate to say inside it, so the whole table is refused
	// rather than only its executable keys.
	if _, ok := doc["adapters"]; ok {
		return trustErrorf(path, "adapters",
			"a project file may reference adapters by name but may not define one; "+
				"adapter definitions belong in user configuration")
	}
	return scanExecutable(path, "", doc)
}

func scanExecutable(path, prefix string, node map[string]any) error {
	// Sorted so a document with two violations always reports the same one.
	for _, key := range slices.Sorted(maps.Keys(node)) {
		full := joinKey(prefix, key)
		if what, forbidden := executableKeys[strings.ToLower(key)]; forbidden {
			return trustErrorf(path, full,
				"a project file may not declare %s; it travels with a clone, so only "+
					"user configuration may say what Recall executes", what)
		}
		if err := scanExecutableValue(path, full, node[key]); err != nil {
			return err
		}
	}
	return nil
}

func scanExecutableValue(path, key string, value any) error {
	switch v := value.(type) {
	case map[string]any:
		return scanExecutable(path, key, v)
	case []map[string]any:
		for i, elem := range v {
			if err := scanExecutable(path, fmt.Sprintf("%s[%d]", key, i), elem); err != nil {
				return err
			}
		}
	case []any:
		for i, elem := range v {
			if err := scanExecutableValue(path, fmt.Sprintf("%s[%d]", key, i), elem); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// parseFile reads one configuration file.
//
// A project file is checked against the trust boundary before it is decoded.
// Unknown keys fail in both layers: an ignored key is a setting the author
// believes is in force, and invariant 6 leaves no room for configuration that
// affects nothing.
func parseFile(path string, layer Layer) (sourceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sourceFile{}, fmt.Errorf("reading configuration: %w", err)
	}

	if layer == LayerProject {
		var doc map[string]any
		if _, err := toml.Decode(string(data), &doc); err != nil {
			return sourceFile{}, invalidErrorf(path, "", "%s", err)
		}
		if err := checkTrustBoundary(path, doc); err != nil {
			return sourceFile{}, err
		}
	}

	var raw rawFile
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return sourceFile{}, invalidErrorf(path, "", "%s", err)
	}
	if err := rejectUnknownKeys(path, md.Undecoded()); err != nil {
		return sourceFile{}, err
	}

	return sourceFile{
		Path:   path,
		Dir:    filepath.Dir(path),
		Layer:  layer,
		Raw:    raw,
		Origin: Origin{Layer: layer, File: path},
	}, nil
}

func rejectUnknownKeys(path string, undecoded []toml.Key) error {
	if len(undecoded) == 0 {
		return nil
	}
	// Keys inside an adapter-owned block are not unknown, they are not ours.
	// The TOML decoder reports everything under a map[string]any as undecoded
	// because it cannot confirm those keys reached a field, and this package
	// deliberately bounds nothing inside settings — the adapter's declared
	// schema does, at the handshake, where it is known.
	//
	// The executable-key check below still runs over them: a settings block is
	// exactly where an executable key would hide, which is why the trust scan
	// reads the raw tree rather than trusting this pass.
	kept := make([]toml.Key, 0, len(undecoded))
	for _, k := range undecoded {
		if adapterOwned(k) && !namesExecutable(k) {
			continue
		}
		kept = append(kept, k)
	}
	if len(kept) == 0 {
		return nil
	}
	sorted := slices.Clone(kept)
	slices.SortFunc(sorted, func(a, b toml.Key) int { return strings.Compare(a.String(), b.String()) })

	keys := make([]string, 0, len(sorted))
	for _, k := range sorted {
		keys = append(keys, k.String())
	}

	first := sorted[0]
	leaf := strings.ToLower(first[len(first)-1])
	if what, forbidden := executableKeys[leaf]; forbidden {
		return invalidErrorf(path, first.String(),
			"%s may be declared only on an adapter definition, never on a source instance", what)
	}
	return invalidErrorf(path, keys[0], "unknown key; the full set is %s", strings.Join(keys, ", "))
}

// adapterOwned reports whether a key lies inside a block this package carries
// but does not interpret.
func adapterOwned(k toml.Key) bool {
	for _, segment := range k[:max(len(k)-1, 0)] {
		switch strings.ToLower(segment) {
		case "settings", "secrets":
			return true
		}
	}
	return false
}

// namesExecutable reports whether any segment of a key is an executable key,
// wherever it appears.
func namesExecutable(k toml.Key) bool {
	for _, segment := range k {
		if _, forbidden := executableKeys[strings.ToLower(segment)]; forbidden {
			return true
		}
	}
	return false
}

// parseAdaptersDir reads $XDG_CONFIG_HOME/recall/adapters.d in lexical order.
//
// The files register adapters and nothing else. Allowing a source or a profile
// here would make the resolved configuration depend on a directory listing,
// and the merge order would stop being something a person can predict.
func parseAdaptersDir(dir string) ([]sourceFile, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	files := make([]sourceFile, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		f, err := parseFile(path, LayerUser)
		if err != nil {
			return nil, err
		}
		switch {
		case len(f.Raw.Sources) > 0:
			return nil, invalidErrorf(path, "sources", "adapters.d files register adapters only")
		case len(f.Raw.Profiles) > 0:
			return nil, invalidErrorf(path, "profiles", "adapters.d files register adapters only")
		case f.Raw.Defaults != nil:
			return nil, invalidErrorf(path, "defaults", "adapters.d files register adapters only")
		}
		files = append(files, f)
	}
	return files, nil
}
