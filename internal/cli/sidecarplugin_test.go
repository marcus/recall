package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The Sidecar plugin surface is tested through a real process, not through
// cli.Run in memory, because the contract it has to satisfy is a process
// contract: one JSON object on stdin, exactly one JSON value on stdout, nothing
// else on stdout, diagnostics on stderr, and exit 0 for an answer of either
// kind. An in-process harness cannot fail the way a plugin fails a host — a
// stray Println, a second write, a non-zero exit — so it would not be testing
// what Sidecar actually depends on.
//
// The corpus is the documents adapter's own fixture corpus, so these tests
// exercise a compiled-in adapter over real files rather than a fake, and the
// environment is the host's documented allowlist rather than the developer's.

var (
	pluginBuildOnce sync.Once
	pluginBuildDir  string
	pluginBinary    string
	pluginBuildErr  error
)

// TestMain owns the one compiled binary these tests share. Building it per test
// would cost more than every assertion in the file put together.
func TestMain(m *testing.M) {
	code := m.Run()
	if pluginBuildDir != "" {
		_ = os.RemoveAll(pluginBuildDir)
	}
	os.Exit(code)
}

func recallBinary(t *testing.T) string {
	t.Helper()
	pluginBuildOnce.Do(func() {
		pluginBuildDir, pluginBuildErr = os.MkdirTemp("", "recall-sidecar-plugin")
		if pluginBuildErr != nil {
			return
		}
		pluginBinary = filepath.Join(pluginBuildDir, "recall")
		build := exec.Command("go", "build", "-o", pluginBinary, "github.com/marcus/recall/cmd/recall")
		if out, err := build.CombinedOutput(); err != nil {
			pluginBuildErr = fmt.Errorf("building recall: %w\n%s", err, out)
		}
	})
	if pluginBuildErr != nil {
		t.Fatal(pluginBuildErr)
	}
	return pluginBinary
}

// pluginMachine is a whole temporary installation: a config home, a state home,
// a cache home, the fixture corpus, and the environment Sidecar actually passes
// a plugin. Nothing here can reach the developer's own recall configuration.
type pluginMachine struct {
	binary string
	env    []string
}

func newPluginMachine(t *testing.T) *pluginMachine { return newPluginMachineWith(t, false) }

// newPluginMachineWith builds the temporary installation. keepUnreadable
// decides whether the corpus keeps the fixture's deliberately unreadable file,
// which is how the degraded path is reached without faking a source.
func newPluginMachineWith(t *testing.T, keepUnreadable bool) *pluginMachine {
	t.Helper()
	binary := recallBinary(t)

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "recall"), 0o755); err != nil {
		t.Fatal(err)
	}
	corpus := copyFixtureCorpus(t, filepath.Join(root, "corpus"), keepUnreadable)
	body := strings.ReplaceAll(sidecarPluginTOML, "CORPUS", corpus)
	if err := os.WriteFile(filepath.Join(configHome, "recall", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly the base allowlist from the protocol document, plus the marker
	// the host sets rather than inherits. If recall ever needed a variable
	// outside this set, a plugin invocation would be the place it broke.
	return &pluginMachine{
		binary: binary,
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + root,
			"TMPDIR=" + filepath.Join(root, "tmp"),
			"XDG_CONFIG_HOME=" + configHome,
			"XDG_STATE_HOME=" + filepath.Join(root, "state"),
			"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
			"SIDECAR_PLUGIN=1",
		},
	}
}

// call runs one invocation the way the host does and enforces the transport
// rules on the way back: exit 0, and exactly one JSON value on stdout.
func (m *pluginMachine) call(t *testing.T, request string) map[string]any {
	t.Helper()

	cmd := exec.Command(m.binary, "sidecar-plugin") //nolint:gosec // the binary is the one this test built
	cmd.Env = m.env
	cmd.Stdin = strings.NewReader(request)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("exit was not 0 for a request the plugin is supposed to answer: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("stdout was not one JSON object: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout carried more than one value; the host reads that as a transport failure\nstdout: %s",
			stdout.String())
	}
	if got := resp["protocol"]; got != sidecarProtocol {
		t.Fatalf("response protocol = %v, want %q", got, sidecarProtocol)
	}
	return resp
}

const sidecarProtocol = "sidecar.plugin/v1-draft"

func sidecarRequest(method, params string) string {
	if params == "" {
		return fmt.Sprintf(`{"protocol":%q,"method":%q,"instance":"recall","deadlineMs":20000}`,
			sidecarProtocol, method)
	}
	return fmt.Sprintf(`{"protocol":%q,"method":%q,"instance":"recall","deadlineMs":20000,"params":%s}`,
		sidecarProtocol, method, params)
}

// TestSidecarPluginDescribeDeclaresTheCanonicalShape pins the declaration
// against the protocol's own canonical describe response, vendored beside this
// test. What is compared is the part Sidecar renders from — the collections and
// their columns — because that is what a host build and a plugin build have to
// agree on; the fixture's matchers and second action belong to the protocol
// example rather than to recall, and are deliberately not asserted.
func TestSidecarPluginDescribeDeclaresTheCanonicalShape(t *testing.T) {
	m := newPluginMachine(t)
	resp := m.call(t, sidecarRequest("describe", ""))

	plugin, ok := resp["plugin"].(map[string]any)
	if !ok {
		t.Fatalf("describe carried no plugin block: %v", resp)
	}
	if plugin["kind"] != "recall" || plugin["name"] != "Recall" {
		t.Errorf("identity = %v, want kind recall and name Recall", plugin)
	}
	if v, _ := plugin["version"].(string); v == "" {
		t.Error("version is empty; the host shows it in the settings page and in plugin list")
	}
	if got := resp["context"]; !equalStrings(got, []string{"project"}) {
		t.Errorf("context = %v, want [project]", got)
	}
	// No matchers. Recall's locator is "<source_id>:<local>", where the source
	// name is user configuration and the local part is adapter-owned, so a
	// pattern wide enough to match it also matches every URL scheme and every
	// "key: value" pair on screen.
	if raw, present := resp["matchers"]; present {
		if list, _ := raw.([]any); len(list) > 0 {
			t.Errorf("matchers = %v, want none for an unstable locator shape", raw)
		}
	}

	got := collectionsByID(t, resp)
	want := collectionsByID(t, loadCanonicalDescribe(t))
	for _, id := range []string{"results", "sources"} {
		gotColumns := normalizeColumns(got[id]["columns"])
		wantColumns := normalizeColumns(want[id]["columns"])
		if !jsonEqual(gotColumns, wantColumns) {
			t.Errorf("%s columns diverged from the canonical shape\n got: %s\nwant: %s",
				id, mustJSON(gotColumns), mustJSON(wantColumns))
		}
	}
	if got["results"]["search"] != "required" {
		t.Errorf("results search = %v, want required", got["results"]["search"])
	}
	if got["sources"]["search"] != "none" {
		t.Errorf("sources search = %v, want none", got["sources"]["search"])
	}
	if got["results"]["detail"] != true || got["sources"]["detail"] != true {
		t.Error("both collections have to declare detail: Enter opens a document on each")
	}
	refresh, _ := got["sources"]["refresh"].(map[string]any)
	if refresh["everySeconds"] != float64(120) {
		t.Errorf("sources refresh = %v, want everySeconds 120", got["sources"]["refresh"])
	}
	if keys := sortKeyIDs(got["results"]["sort"]); !equalStrings(keys, []string{"rank", "source", "updated"}) {
		t.Errorf("results sort keys = %v, want rank, source, updated", keys)
	}

	actions, _ := resp["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v, want exactly refresh-source", resp["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if action["id"] != "refresh-source" || action["on"] != "item" ||
		action["collection"] != "sources" || action["mutates"] != true || action["confirm"] != true {
		t.Errorf("action = %v, want refresh-source on a sources item, mutating and confirmed", action)
	}
}

// TestSidecarPluginListAndGetRoundTrip is the whole user gesture: a query
// returns rows, and a row's id is the handle that opens it. A list whose ids
// did not round-trip through get would be a list of things nobody can open.
func TestSidecarPluginListAndGetRoundTrip(t *testing.T) {
	m := newPluginMachine(t)

	listed := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`))
	page := mustPage(t, listed)
	if page["outcome"] != "answered" {
		t.Fatalf("outcome = %v, want answered over the fixture corpus: %s", page["outcome"], mustJSON(listed))
	}
	items, _ := page["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("no rows for a term the fixture corpus holds: %s", mustJSON(listed))
	}
	first, _ := items[0].(map[string]any)
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("row carries no id, so it could be neither opened nor acted on: %v", first)
	}
	cells, _ := first["cells"].(map[string]any)
	for _, column := range []string{"rank", "title", "source", "excerpt"} {
		if _, present := cells[column]; !present {
			t.Errorf("row has no %q cell: %v", column, cells)
		}
	}
	if cells["rank"] != "1" {
		t.Errorf("first row rank = %v, want 1", cells["rank"])
	}

	opened := m.call(t, sidecarRequest("get",
		fmt.Sprintf(`{"collection":"results","id":%q}`, id)))
	doc, ok := opened["resource"].(map[string]any)
	if !ok {
		t.Fatalf("get returned no resource: %s", mustJSON(opened))
	}
	if doc["identity"] != id {
		t.Errorf("identity = %v, want the row id %q back", doc["identity"], id)
	}
	if body := sectionBody(doc, "Evidence"); !strings.Contains(body, "orroboration") {
		t.Errorf("the Evidence section does not carry the record's text: %s", mustJSON(doc))
	}
}

// TestSidecarPluginListsSourcesAndOpensOne covers the second collection, which
// is what the host shows when nothing is open: the sources a query would reach,
// and their health.
func TestSidecarPluginListsSourcesAndOpensOne(t *testing.T) {
	m := newPluginMachine(t)

	listed := m.call(t, sidecarRequest("list",
		`{"collection":"sources","query":"","view":"","sort":{"key":"","dir":""},"cursor":"","limit":100}`))
	page := mustPage(t, listed)
	items, _ := page["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("no source rows for a configured profile: %s", mustJSON(listed))
	}
	row, _ := items[0].(map[string]any)
	cells, _ := row["cells"].(map[string]any)
	if cells["name"] != "docs" {
		t.Errorf("first source row = %v, want the configured docs source", cells)
	}
	if cells["health"] == "" {
		t.Error("a source row with no health reads as a source nobody asked")
	}

	opened := m.call(t, sidecarRequest("get", `{"collection":"sources","id":"docs"}`))
	doc, ok := opened["resource"].(map[string]any)
	if !ok {
		t.Fatalf("get on a source returned no resource: %s", mustJSON(opened))
	}
	if doc["identity"] != "docs" {
		t.Errorf("identity = %v, want docs", doc["identity"])
	}
	if len(sectionsOf(doc)) == 0 {
		t.Errorf("a source document with no sections says nothing an operator can act on: %s", mustJSON(doc))
	}
}

// TestSidecarPluginAbstainsOnAnEmptyQuery.
//
// The host answers an empty query on a search: required collection without
// starting a process, so this path is only reached by hand — and the answer has
// to be the same one. It is abstained rather than answered because an empty
// list from a query nobody made is not a claim about the corpus.
func TestSidecarPluginAbstainsOnAnEmptyQuery(t *testing.T) {
	m := newPluginMachine(t)
	resp := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`))
	page := mustPage(t, resp)
	if page["outcome"] != "abstained" {
		t.Errorf("outcome = %v, want abstained: %s", page["outcome"], mustJSON(resp))
	}
	if items, _ := page["items"].([]any); len(items) != 0 {
		t.Errorf("an empty query produced rows: %s", mustJSON(resp))
	}
}

// TestSidecarPluginRefusesWhatItCannotAnswer. Each of these is a typed failure
// and each exits 0: a plugin that exited non-zero here would be reported to the
// user as a crashed executable rather than as a request it declined.
func TestSidecarPluginRefusesWhatItCannotAnswer(t *testing.T) {
	m := newPluginMachine(t)

	tests := map[string]struct {
		request string
		wantIn  []string
	}{
		"unknown method": {
			request: sidecarRequest("enumerate", ""),
			wantIn:  []string{"enumerate", sidecarProtocol},
		},
		"wrong protocol": {
			request: `{"protocol":"sidecar.plugin/v9","method":"describe","instance":"recall","deadlineMs":5000}`,
			wantIn:  []string{"sidecar.plugin/v9", sidecarProtocol},
		},
		"unknown collection": {
			request: sidecarRequest("list", `{"collection":"people","query":"x","limit":10}`),
			wantIn:  []string{"people", "results", "sources"},
		},
		"not one JSON object": {
			request: "this is not JSON",
			wantIn:  []string{"JSON"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			resp := m.call(t, tc.request)
			failure, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("no typed error came back: %s", mustJSON(resp))
			}
			if failure["code"] != "invalid_request" {
				t.Errorf("code = %v, want invalid_request", failure["code"])
			}
			message, _ := failure["message"].(string)
			for _, want := range tc.wantIn {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not name %q", message, want)
				}
			}
		})
	}
}

// TestSidecarPluginReportsAnUnconfiguredInstall. An installed but unconfigured
// plugin is the state a first run is in, and the protocol asks for a typed
// invalid_config carrying the one line that fixes it.
func TestSidecarPluginReportsAnUnconfiguredInstall(t *testing.T) {
	binary := recallBinary(t)
	root := t.TempDir()

	cmd := exec.Command(binary, "sidecar-plugin") //nolint:gosec // the binary is the one this test built
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"SIDECAR_PLUGIN=1",
	}
	cmd.Stdin = strings.NewReader(sidecarRequest("describe", ""))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("an unconfigured install exited non-zero: %v\n%s", err, stderr.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("stdout was not one JSON object: %v\n%s", err, stdout.String())
	}
	failure, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("an unconfigured install answered as though it were configured: %s", stdout.String())
	}
	if failure["code"] != "invalid_config" {
		t.Errorf("code = %v, want invalid_config", failure["code"])
	}
	hint, _ := failure["setupHint"].(string)
	if !strings.Contains(hint, "recall init") {
		t.Errorf("setupHint = %q, want the command that configures recall", hint)
	}
}

// TestSidecarPluginReportsDegradedCoverage. The fixture corpus carries one file
// the adapter cannot read, so the source answers partially — which is the whole
// distinction this protocol's outcome exists to carry. The page is still a
// page: the host never blanks a list to explain, and a caller must be able to
// tell "these are the matches" from "these are the matches the sources that
// answered hold".
func TestSidecarPluginReportsDegradedCoverage(t *testing.T) {
	m := newPluginMachineWith(t, true)
	resp := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`))
	page := mustPage(t, resp)
	if page["outcome"] != "degraded" {
		t.Fatalf("outcome = %v, want degraded when a source could only answer partially: %s",
			page["outcome"], mustJSON(resp))
	}
	if items, _ := page["items"].([]any); len(items) == 0 {
		t.Error("a degraded page dropped its rows; incomplete coverage is not an empty answer")
	}
	notices, _ := page["notices"].([]any)
	if len(notices) == 0 {
		t.Fatalf("degraded coverage with no notice leaves the user to infer it: %s", mustJSON(resp))
	}
	notice, _ := notices[0].(map[string]any)
	text, _ := notice["text"].(string)
	if !strings.Contains(text, "docs") || !strings.Contains(text, "did not answer") {
		t.Errorf("notice %q does not name the source that could not answer", text)
	}
}

// --- helpers ---------------------------------------------------------------

// copyFixtureCorpus copies the documents adapter's fixture corpus into the
// temporary machine, so a test can vary it without editing shared testdata.
func copyFixtureCorpus(t *testing.T, dst string, keepUnreadable bool) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "adapters", "docs", "testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if !keepUnreadable && strings.HasPrefix(rel, "broken") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func mustPage(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	page, ok := resp["page"].(map[string]any)
	if !ok {
		t.Fatalf("no page came back: %s", mustJSON(resp))
	}
	return page
}

func collectionsByID(t *testing.T, resp map[string]any) map[string]map[string]any {
	t.Helper()
	raw, _ := resp["collections"].([]any)
	out := map[string]map[string]any{}
	for _, entry := range raw {
		c, _ := entry.(map[string]any)
		id, _ := c["id"].(string)
		out[id] = c
	}
	for _, want := range []string{"results", "sources"} {
		if out[want] == nil {
			t.Fatalf("no %q collection: %s", want, mustJSON(resp))
		}
	}
	return out
}

// normalizeColumns drops the keys whose absence and whose false value mean the
// same thing, so a column written with explicit defaults compares equal to one
// written without them.
func normalizeColumns(raw any) []map[string]any {
	list, _ := raw.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		column, _ := entry.(map[string]any)
		clean := map[string]any{}
		for key, value := range column {
			switch value {
			case false, float64(0), "":
				continue
			}
			clean[key] = value
		}
		out = append(out, clean)
	}
	return out
}

func sortKeyIDs(raw any) []string {
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, entry := range list {
		key, _ := entry.(map[string]any)
		id, _ := key["id"].(string)
		out = append(out, id)
	}
	return out
}

func sectionsOf(doc map[string]any) []any {
	sections, _ := doc["sections"].([]any)
	return sections
}

func sectionBody(doc map[string]any, title string) string {
	for _, entry := range sectionsOf(doc) {
		section, _ := entry.(map[string]any)
		if section["title"] != title {
			continue
		}
		body, _ := section["body"].(map[string]any)
		text, _ := body["text"].(string)
		return text
	}
	return ""
}

func loadCanonicalDescribe(t *testing.T) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "sidecar-plugin-describe.canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func equalStrings(got any, want []string) bool {
	switch v := got.(type) {
	case []string:
		return jsonEqual(v, want)
	case []any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			s, _ := entry.(string)
			out = append(out, s)
		}
		return jsonEqual(out, want)
	default:
		return false
	}
}

func jsonEqual(a, b any) bool { return mustJSON(a) == mustJSON(b) }

func mustJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(body)
}

// sidecarPluginTOML is one indexed source over the documents adapter's fixture
// corpus: a real compiled-in adapter over real files, so what these tests drive
// is the same path a user's first install takes.
const sidecarPluginTOML = `
[defaults]
profile = "work"
timeout_ms = 20000

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "documents"
location = "CORPUS"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs"]
max_sensitivity = "internal"
`
