package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
)

func TestInitCreatesAUsableSecureConfiguration(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(h.root, "config", "recall")
	if err := os.Remove(configDir); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := h.run("init", "--docs", docs)
	if code != cli.ExitOK {
		t.Fatalf("init exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	configPath := filepath.Join(configDir, "config.toml")
	for _, want := range []string{
		"created " + configPath,
		"documents source docs (",
		"documents directory " + docs,
		"recall refresh --source docs",
		`recall query "what did we decide"`,
	} {
		contains(t, stdout, want, "init must report what it wrote and the next two commands")
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("new config directory mode = %o, want 700", got)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Errorf("atomic creation left temporary files: %v", entries)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`adapter = "documents"`,
		`location = "` + docs + `"`,
		`# Tasks source — requires the tasks CLI on PATH and a Tasks workspace directory.`,
		`# td source — requires the td CLI on PATH and a td workspace directory.`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("generated config does not contain %q\n%s", want, body)
		}
	}

	code, explained, stderr := h.run("config", "explain", "--json")
	if code != cli.ExitOK {
		t.Fatalf("generated config did not load: exit %d\n%s\n%s", code, explained, stderr)
	}
	for _, want := range []string{`"source_id": "docs"`, `"value": "documents"`, docs} {
		contains(t, explained, want, "generated configuration must resolve through the real loader")
	}
}

func TestInitJSONReportsWhatAndWhere(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := h.run("init", "--json", "--docs", docs)
	if code != cli.ExitOK {
		t.Fatalf("init --json exit %d: %s", code, stderr)
	}
	var got struct {
		Action     string `json:"action"`
		ConfigPath string `json:"config_path"`
		Documents  struct {
			SourceID  string `json:"source_id"`
			SourceUID string `json:"source_uid"`
			Location  string `json:"location"`
		} `json:"documents"`
		Next []string `json:"next"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("init --json is not JSON: %v\n%s", err, stdout)
	}
	if got.Action != "created" ||
		got.ConfigPath != filepath.Join(h.root, "config", "recall", "config.toml") ||
		got.Documents.SourceID != "docs" ||
		got.Documents.Location != docs ||
		len(got.Documents.SourceUID) != 16 ||
		len(got.Next) != 2 {
		t.Fatalf("unexpected init result: %+v", got)
	}
}

func TestInitRefusesToOverwriteUnlessForced(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(h.root, "config", "recall", "config.toml")
	const original = "# keep me\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := h.run("init", "--docs", docs)
	if code != cli.ExitError {
		t.Fatalf("init over existing file exit = %d, want %d", code, cli.ExitError)
	}
	contains(t, stderr, "already exists; pass --force", "refusal must name the explicit escape hatch")
	if body, err := os.ReadFile(configPath); err != nil || string(body) != original {
		t.Fatalf("refused init changed existing config: %q, %v", body, err)
	}

	code, stdout, stderr := h.run("init", "--docs", docs, "--force", "--json")
	if code != cli.ExitOK {
		t.Fatalf("forced init exit %d: %s", code, stderr)
	}
	contains(t, stdout, `"action": "replaced"`, "JSON must distinguish creation from replacement")
	if body, err := os.ReadFile(configPath); err != nil || !strings.Contains(string(body), `adapter = "documents"`) {
		t.Fatalf("forced init did not replace config: %v\n%s", err, body)
	}
}

func TestInitForceReportsCreatedWhenNothingWasReplaced(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := h.run("init", "--docs", docs, "--force", "--json")
	if code != cli.ExitOK {
		t.Fatalf("forced first init exit %d: %s", code, stderr)
	}
	contains(t, stdout, `"action": "created"`,
		"--force must not claim it replaced a file that did not exist")
}

func TestInitValidatesItsNonInteractiveBoundaryBeforeWriting(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.env.Stdin = strings.NewReader("")
	configPath := filepath.Join(h.root, "config", "recall", "config.toml")

	code, _, stderr := h.run("init")
	if code != cli.ExitError {
		t.Fatalf("init without --docs exit = %d, want %d", code, cli.ExitError)
	}
	contains(t, stderr, "--docs is required with --json or when stdin is not an interactive terminal",
		"an agent invocation must fail rather than block for input")

	code, _, stderr = h.run("init", "--docs", filepath.Join(h.root, "missing"))
	if code != cli.ExitError {
		t.Fatalf("init with missing docs exit = %d, want %d", code, cli.ExitError)
	}
	contains(t, stderr, "documents directory", "invalid source path should be actionable")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("invalid init wrote a configuration: %v", err)
	}
}

func TestInitTreatsDevNullAsNonInteractive(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	h.env.Stdin = devNull

	code, stdout, stderr := h.run("init")
	if code != cli.ExitError {
		t.Fatalf("init from %s exit = %d, want %d", os.DevNull, code, cli.ExitError)
	}
	if strings.Contains(stdout, "Documents directory:") {
		t.Fatalf("init prompted with non-terminal stdin: %q", stdout)
	}
	contains(t, stderr, "--docs is required with --json or when stdin is not an interactive terminal",
		"cron and launchd commonly attach stdin to the null device")
}

func TestInitPreservesExplicitDirectoryWhitespace(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents ")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := h.run("init", "--docs", docs)
	if code != cli.ExitOK {
		t.Fatalf("init with whitespace-bearing directory exit %d: %s", code, stderr)
	}
	body, err := os.ReadFile(filepath.Join(h.root, "config", "recall", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `location = "`+docs+`"`) {
		t.Fatalf("generated config changed the explicit path %q:\n%s", docs, body)
	}
}

func TestInitCleanMachineLoop(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	docs := filepath.Join(h.root, "documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	const evidence = "Use a calibrated spectrometer when recording aurora emissions."
	if err := os.WriteFile(
		filepath.Join(docs, "field-guide.md"),
		[]byte("# Field guide\n\n## Instrument notes\n\n"+evidence+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	assertOK := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := h.run(args...)
		if code != cli.ExitOK {
			t.Fatalf("recall %s: exit %d\nstdout:\n%s\nstderr:\n%s",
				strings.Join(args, " "), code, stdout, stderr)
		}
		return stdout
	}

	assertOK("init", "--docs", docs)
	refresh := assertOK("refresh", "--source", "docs")
	contains(t, refresh, "outcome refreshed", "first-run refresh must publish a usable index")

	query := assertOK("query", "calibrated spectrometer")
	contains(t, query, "outcome answered  coverage complete", "the initialized source must answer")
	contains(t, query, "docs:field-guide.md#L", "the first answer must be an expandable pointer")

	locator := ""
	for _, field := range strings.Fields(query) {
		if strings.HasPrefix(field, "docs:field-guide.md#L") {
			locator = field
			break
		}
	}
	if locator == "" {
		t.Fatalf("query returned no locator\n%s", query)
	}
	expanded := assertOK("expand", "--detail", "full", locator)
	contains(t, expanded, evidence, "the pointer from query must expand on the same installation")

	doctor := assertOK("doctor")
	contains(t, doctor, "status ok  profile default", "the initialized machine must be healthy")
}
