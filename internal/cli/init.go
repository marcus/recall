package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/recall"
)

const initHelp = `usage: recall init --docs <directory> [flags]

Create the first user configuration at the resolved XDG config path. The
documents source is active; the other built-in source examples stay commented
until their command and workspace requirements are present.

Without --docs, an interactive terminal prompts for the directory. Scripts and
agents must pass --docs so the command is deterministic and never waits.

flags:
  --docs DIR  directory of Markdown documents to index
  --force     replace an existing config.toml atomically
  --json      report the created file and next commands as JSON

` + exitCodes

type initDocuments struct {
	SourceID  string           `json:"source_id"`
	SourceUID recall.SourceUID `json:"source_uid"`
	Location  string           `json:"location"`
}

type initResult struct {
	Action     string        `json:"action"`
	ConfigPath string        `json:"config_path"`
	Documents  initDocuments `json:"documents"`
	Next       []string      `json:"next"`
}

func runInit(env Env, args []string) int {
	fs := newFlagSet("init")
	var (
		docsPath = fs.String("docs", "", "directory of Markdown documents to index")
		force    = fs.Bool("force", false, "replace an existing config.toml")
		asJSON   = fs.Bool("json", false, "emit JSON")
	)
	if ok, code := parse(env, fs, initHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, initHelp, fmt.Errorf("init takes no arguments; use --docs DIR"))
	}

	docs, err := resolveInitDocs(env, *docsPath, !*asJSON)
	if err != nil {
		return usageErr(env, initHelp, err)
	}
	paths, err := initPaths(env.Paths)
	if err != nil {
		fail(env, err)
		return ExitError
	}

	ids := make([]recall.SourceUID, 3)
	for i := range ids {
		ids[i], err = config.GenerateSourceUID()
		if err != nil {
			fail(env, err)
			return ExitError
		}
	}
	body := initConfig(docs, ids[0], ids[1], ids[2])
	configPath := paths.ConfigFile()
	action, err := writeConfigAtomic(configPath, []byte(body), *force)
	if err != nil {
		fail(env, err)
		return ExitError
	}

	result := initResult{
		Action:     action,
		ConfigPath: configPath,
		Documents: initDocuments{
			SourceID:  "docs",
			SourceUID: ids[0],
			Location:  docs,
		},
		Next: []string{
			"recall refresh --source docs",
			`recall query "what did we decide"`,
		},
	}
	if *asJSON {
		return report(env, emitJSON(env.Stdout, result))
	}
	var out out
	out.printf("%s %s\n", result.Action, result.ConfigPath)
	out.printf("documents source %s (%s)\n", result.Documents.SourceID, result.Documents.SourceUID)
	out.printf("documents directory %s\n", result.Documents.Location)
	out.blank()
	out.line("Next:")
	for _, command := range result.Next {
		out.printf("  %s\n", command)
	}
	return report(env, out.flush(env.Stdout))
}

func resolveInitDocs(env Env, asked string, allowPrompt bool) (string, error) {
	if asked == "" {
		if !allowPrompt || !interactiveReader(env.stdin()) {
			return "", fmt.Errorf("--docs is required with --json or when stdin is not an interactive terminal")
		}
		if _, err := io.WriteString(env.Stdout, "Documents directory: "); err != nil {
			return "", err
		}
		line, err := bufio.NewReader(env.stdin()).ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", fmt.Errorf("reading documents directory: %w", err)
		}
		asked = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.TrimSpace(asked) == "" {
			return "", fmt.Errorf("documents directory must not be blank")
		}
	}

	expanded, err := expandInitHome(asked)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolving documents directory %q: %w", asked, err)
	}
	abs = filepath.Clean(abs)
	if !utf8.ValidString(abs) {
		return "", fmt.Errorf("documents directory %q is not valid UTF-8 and cannot be written to TOML", abs)
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return "", fmt.Errorf("documents directory %s: %w", abs, err)
	case !info.IsDir():
		return "", fmt.Errorf("documents path %s is not a directory", abs)
	}
	return abs, nil
}

func interactiveReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && isTerminal(f)
}

func expandInitHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", path, err)
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
}

func initPaths(paths config.Paths) (config.Paths, error) {
	switch {
	case paths.ConfigHome == "" && paths.StateHome == "" && paths.CacheHome == "":
		return config.XDGPaths()
	case paths.ConfigHome != "" && paths.StateHome != "" && paths.CacheHome != "":
		return paths, nil
	default:
		return config.Paths{}, fmt.Errorf("initializing config: Paths must be empty or fully specified")
	}
}

func initConfig(
	docs string,
	docsUID, tasksUID, tdUID recall.SourceUID,
) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `# Recall user configuration.
# Run "recall config explain" to see this file merged with any project recall.toml.

[defaults]
profile = "default"

[[sources]]
source_uid = %s
source_id = "docs"
adapter = "documents"
location = %s
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.default]
sources = ["docs"]
max_sensitivity = "internal"

# Tasks source — requires the tasks CLI on PATH and a Tasks workspace directory.
# [[sources]]
# source_uid = %s
# source_id = "tasks"
# adapter = "tasks"
# location = "/path/to/tasks-workspace"
# location_kind = "path"
# freshness_mode = "live"
# sensitivity = "internal"
# base_prior = 1.0

# td source — requires the td CLI on PATH and a td workspace directory.
# [[sources]]
# source_uid = %s
# source_id = "td"
# adapter = "td"
# location = "/path/to/td-workspace"
# location_kind = "path"
# freshness_mode = "live"
# sensitivity = "internal"
# base_prior = 1.0
`,
		tomlString(string(docsUID)),
		tomlString(docs),
		tomlString(string(tasksUID)),
		tomlString(string(tdUID)),
	)
	return b.String()
}

func tomlString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\u%04X`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func writeConfigAtomic(path string, body []byte, force bool) (action string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating config directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config.toml.tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary config in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("securing temporary config: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return "", fmt.Errorf("writing temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("syncing temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing temporary config: %w", err)
	}

	// Publishing by hard link is atomic and tells us whether the destination
	// existed at the exact publication boundary. That keeps --force's result
	// truthful even when another initializer races this one.
	switch err := os.Link(tmpPath, path); {
	case err == nil:
		action = "created"
		if err := os.Remove(tmpPath); err != nil {
			return "", fmt.Errorf("removing temporary config: %w", err)
		}
	case os.IsExist(err) && force:
		if err := os.Rename(tmpPath, path); err != nil {
			return "", fmt.Errorf("replacing %s: %w", path, err)
		}
		action = "replaced"
	case os.IsExist(err):
		return "", fmt.Errorf("%s already exists; pass --force to replace it", path)
	default:
		return "", fmt.Errorf("creating %s: %w", path, err)
	}

	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return action, nil
}
