package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
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
	return newPluginMachineWithFiles(t, keepUnreadable, nil)
}

// newPluginMachineWithFiles is the same installation with extra documents
// written into the corpus, keyed by path relative to it. It exists so a test
// can put text no fixture would carry — terminal control sequences, a
// paragraph longer than a cell — in front of a real adapter rather than
// asserting against a fake.
func newPluginMachineWithFiles(t *testing.T, keepUnreadable bool, extra map[string]string) *pluginMachine {
	return newPluginMachineConfigured(t, sidecarPluginTOML, keepUnreadable, extra)
}

// newPluginMachineConfigured is the same installation over a caller-supplied
// configuration. The filter tests need more than one profile, more than one
// source, and declared record types to have anything to choose between, and a
// chooser built from configuration can only be tested against a configuration.
//
// Three placeholders are substituted: CORPUS is the fixture corpus, TRANSCRIPTS
// is a second small corpus (a separate store, so two sources are two sources
// rather than one seen twice), and MISSING is a path that does not exist.
func newPluginMachineConfigured(
	t *testing.T, configTOML string, keepUnreadable bool, extra map[string]string,
) *pluginMachine {
	t.Helper()
	binary := recallBinary(t)

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "recall"), 0o755); err != nil {
		t.Fatal(err)
	}
	corpus := copyFixtureCorpus(t, filepath.Join(root, "corpus"), keepUnreadable)
	for name, body := range extra {
		target := filepath.Join(corpus, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	transcripts := filepath.Join(root, "transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "standup.md"),
		[]byte(sidecarTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer(
		"CORPUS", corpus,
		"TRANSCRIPTS", transcripts,
		"MISSING", filepath.Join(root, "gone"),
	).Replace(configTOML)
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
// rules on the way back: exit 0, exactly one JSON value on stdout, and no
// response on the wrong stream.
func (m *pluginMachine) call(t *testing.T, request string) map[string]any {
	t.Helper()
	resp, _ := m.callRaw(t, request)
	return resp
}

// callPinned is call for an installation whose configured argv carries flags —
// `recall sidecar-plugin --profile NAME` is how an installation pins the plugin
// to one profile, and every method has to agree about which one that is.
func (m *pluginMachine) callPinned(t *testing.T, profile, request string) map[string]any {
	t.Helper()
	resp, _ := m.callRaw(t, request, "--profile", profile)
	return resp
}

// callRaw is call plus the raw stdout bytes, for the assertions that are about
// the bytes rather than the decoded object.
func (m *pluginMachine) callRaw(t *testing.T, request string, args ...string) (map[string]any, []byte) {
	t.Helper()
	return m.callRawOn(t, sidecarProtocol, request, args...)
}

// callRawOn is callRaw for a request that asks on a specific protocol
// identifier and must be answered on that same one. A plugin that answered the
// frozen identifier to a host still asking the draft would be a protocol
// failure at that host, so the identifier is asserted rather than accepted.
func (m *pluginMachine) callRawOn(
	t *testing.T, wantProtocol, request string, args ...string,
) (map[string]any, []byte) {
	t.Helper()

	cmd := exec.Command(m.binary, append([]string{"sidecar-plugin"}, args...)...) //nolint:gosec // the binary is the one this test built
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
	if got := resp["protocol"]; got != wantProtocol {
		t.Fatalf("response protocol = %v, want %q", got, wantProtocol)
	}
	// The host drains stderr and discards it, so a response written there is
	// a response nobody reads. Diagnostics are welcome; an answer is not.
	if strings.Contains(stderr.String(), wantProtocol) {
		t.Fatalf("a protocol response reached stderr, which the host discards:\n%s", stderr.String())
	}
	return resp, stdout.Bytes()
}

// The frozen identifier, and the pre-freeze one a Sidecar older than the freeze
// still asks on. Both are spelled literally here rather than imported, because
// what these tests hold recall to is the wire, not a constant it shares.
const (
	sidecarProtocol      = "sidecar.plugin/v1"
	sidecarProtocolDraft = "sidecar.plugin/v1-draft"
)

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

	canonical := loadCanonicalDescribe(t)
	// The vendored copy has to be a copy of the frozen protocol's own response,
	// and the identifier is the part of it that goes stale silently: a fixture
	// left on the pre-freeze identifier still parses, still compares equal on
	// every field asserted below, and is no longer the protocol.
	if got, want := resp["protocol"], canonical["protocol"]; got != want {
		t.Errorf("describe answered on %v; the vendored canonical response is %v", got, want)
	}

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
	want := collectionsByID(t, canonical)
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

// TestSidecarPluginAnswersBothProtocolIdentifiers.
//
// The identifier froze as sidecar.plugin/v1, and a Sidecar built before the
// freeze asks on sidecar.plugin/v1-draft. Recall answers both and stamps the
// response with the one it was asked, so upgrading recall never requires
// upgrading Sidecar first — and a host that validates the identifier strictly,
// as Sidecar does, still sees its own back.
func TestSidecarPluginAnswersBothProtocolIdentifiers(t *testing.T) {
	m := newPluginMachine(t)

	for _, identifier := range []string{sidecarProtocol, sidecarProtocolDraft} {
		t.Run(identifier, func(t *testing.T) {
			// callRawOn asserts the echo: the response has to carry this
			// identifier and no other.
			described, _ := m.callRawOn(t, identifier, fmt.Sprintf(
				`{"protocol":%q,"method":"describe","instance":"recall","deadlineMs":20000}`, identifier))
			plugin, ok := described["plugin"].(map[string]any)
			if !ok || plugin["kind"] != "recall" {
				t.Fatalf("describe on %s did not answer: %s", identifier, mustJSON(described))
			}

			// Not only describe: the identifier is checked once, for every
			// method, so a real query has to come back on it too.
			listed, _ := m.callRawOn(t, identifier, fmt.Sprintf(
				`{"protocol":%q,"method":"list","instance":"recall","deadlineMs":20000,`+
					`"params":{"collection":"results","query":"corroboration","limit":20}}`, identifier))
			page := mustPage(t, listed)
			if items, _ := page["items"].([]any); len(items) == 0 {
				t.Errorf("list on %s answered no rows: %s", identifier, mustJSON(listed))
			}

			// A refusal is a response, and the host validates its identifier
			// the same way it validates an answer's. A typed failure stamped
			// with the other identifier would reach a pre-freeze Sidecar as a
			// crashed executable instead of as the sentence it carries.
			refused, _ := m.callRawOn(t, identifier, fmt.Sprintf(
				`{"protocol":%q,"method":"get","instance":"recall","deadlineMs":20000,`+
					`"params":{"collection":"people","id":"x"}}`, identifier))
			failure, ok := refused["error"].(map[string]any)
			if !ok || failure["code"] != "invalid_request" {
				t.Errorf("an unknown collection on %s was not a typed refusal: %s",
					identifier, mustJSON(refused))
			}
		})
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

// TestSidecarPluginNeverSendsTerminalControlSequences. Every string in a
// response is provider text: it came from a document recall did not write, and
// the host paints it into a table and a document body. A cell carrying an
// escape sequence would be a plugin choosing a colour, moving a cursor, or
// setting a window title through content the user only meant to read. The host
// sanitizes too, and this asserts recall does not lean on that: a plugin whose
// only defence is the host's is a plugin that is unsafe on the next host.
func TestSidecarPluginNeverSendsTerminalControlSequences(t *testing.T) {
	hostile := "# Kestrel \x1b[31mtelemetry\x1b[0m notes\x07\n\n" +
		"The \x1b]0;window-title-hijack\x07 kestrel pipeline\x01\x02 emits \x7f rows " +
		"about kestrel corroboration.\n"
	m := newPluginMachineWithFiles(t, false, map[string]string{"kestrel.md": hostile})

	listed := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"kestrel","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`))
	page := mustPage(t, listed)
	items, _ := page["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("no rows for a term the corpus holds: %s", mustJSON(listed))
	}
	first, _ := items[0].(map[string]any)
	id, _ := first["id"].(string)
	assertNoControlRunes(t, "list response", listed)
	// A cell is one line by construction, so newline and tab are control
	// characters there too: the host draws a row, not a paragraph.
	for name, raw := range first["cells"].(map[string]any) {
		if value, _ := raw.(string); strings.ContainsAny(value, "\n\r\t") {
			t.Errorf("cell %q carries a line break: %q", name, value)
		}
	}

	opened := m.call(t, sidecarRequest("get", fmt.Sprintf(`{"collection":"results","id":%q}`, id)))
	assertNoControlRunes(t, "get response", opened)
	doc, ok := opened["resource"].(map[string]any)
	if !ok {
		t.Fatalf("get returned no resource: %s", mustJSON(opened))
	}
	if body := sectionBody(doc, "Evidence"); !strings.Contains(body, "kestrel") {
		t.Errorf("the Evidence section lost the record's text along with the escapes: %s", mustJSON(opened))
	}
}

// TestSidecarPluginBoundsEveryCell. The protocol's cell bound is 512
// characters and the host truncates past it, but a plugin that streamed a
// whole document into one cell would be sending bytes nobody can ever paint
// and paying for them on every keystroke of a live search.
func TestSidecarPluginBoundsEveryCell(t *testing.T) {
	long := strings.Repeat("kestrel migration telemetry corroboration ", 400)
	m := newPluginMachineWithFiles(t, false, map[string]string{
		"long.md": "# " + strings.Repeat("Kestrel ", 200) + "\n\n" + long + "\n",
	})

	listed := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"kestrel","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`))
	page := mustPage(t, listed)
	items, _ := page["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("no rows for a term the corpus holds: %s", mustJSON(listed))
	}
	for _, entry := range items {
		row, _ := entry.(map[string]any)
		cells, _ := row["cells"].(map[string]any)
		for name, raw := range cells {
			value, _ := raw.(string)
			if n := len([]rune(value)); n > 512 {
				t.Errorf("cell %q is %d runes; the protocol's bound is 512", name, n)
			}
		}
	}
	for _, entry := range notices(page) {
		if n := len([]rune(entry)); n > 200 {
			t.Errorf("notice is %d runes; the protocol's bound is 200", n)
		}
	}
	if n := len(notices(page)); n > 4 {
		t.Errorf("page carries %d notices; the protocol's bound is 4", n)
	}
}

// TestSidecarPluginLocatorsAreStableAcrossInstalls. A row id is what `get` and
// every item action receive, and Sidecar persists the open document's identity
// in pane state across a relaunch. An id carrying a temp path, a run counter,
// or an index offset would reopen as not_found the next morning, so two
// independent installations over the same documents have to name them
// identically.
func TestSidecarPluginLocatorsAreStableAcrossInstalls(t *testing.T) {
	request := sidecarRequest("list",
		`{"collection":"results","query":"corroboration","view":"","sort":{"key":"","dir":""},"cursor":"","limit":20}`)

	first := newPluginMachine(t)
	second := newPluginMachine(t)
	firstIDs := itemIDs(t, mustPage(t, first.call(t, request)))
	secondIDs := itemIDs(t, mustPage(t, second.call(t, request)))

	if len(firstIDs) == 0 {
		t.Fatal("no rows for a term the fixture corpus holds")
	}
	if !jsonEqual(firstIDs, secondIDs) {
		t.Fatalf("two installations over the same corpus named the same records differently\n%v\n%v",
			firstIDs, secondIDs)
	}
	// Same machine, second run: the id must not move because the query ran again.
	if again := itemIDs(t, mustPage(t, first.call(t, request))); !jsonEqual(firstIDs, again) {
		t.Errorf("ids moved between two runs of one installation\n%v\n%v", firstIDs, again)
	}
	for _, id := range firstIDs {
		if strings.Contains(id, os.TempDir()) || filepath.IsAbs(id) {
			t.Errorf("id %q carries this installation's own path, so it cannot survive a reinstall", id)
		}
	}
	// And the id an installation never produced still opens on it, which is
	// what makes a persisted document identity worth persisting.
	opened := second.call(t, sidecarRequest("get",
		fmt.Sprintf(`{"collection":"results","id":%q}`, firstIDs[0])))
	doc, ok := opened["resource"].(map[string]any)
	if !ok || doc["identity"] != firstIDs[0] {
		t.Errorf("an id from another installation did not open: %s", mustJSON(opened))
	}
}

// TestSidecarPluginRefusesARemoteBoundSurface. On a remote-bound surface the
// project context carries another machine's paths and that machine's host ID.
// This recall indexes the machine it runs on, so answering would report one
// checkout's evidence as another's — the protocol's rule is to say so by name.
func TestSidecarPluginRefusesARemoteBoundSurface(t *testing.T) {
	m := newPluginMachine(t)
	remote := `"context":{"project":{"root":"/checkout","workDir":"/checkout","name":"sidecar","branch":"main","hostId":"workshop"}}`

	// Every method that carries context, not only the two over the results
	// collection. A source list is a fact about a machine, so from a pane bound
	// to another one it is the wrong machine's list — and refresh-source would
	// reindex a corpus nobody in that pane is looking at.
	for name, params := range map[string]string{
		"list":         `"method":"list","params":{"collection":"results","query":"corroboration","limit":20}`,
		"get":          `"method":"get","params":{"collection":"results","id":"docs:projects/recall/architecture.md#L11-L14"}`,
		"list sources": `"method":"list","params":{"collection":"sources"}`,
		"get source":   `"method":"get","params":{"collection":"sources","id":"docs"}`,
		"act":          `"method":"act","params":{"action":"refresh-source","collection":"sources","id":"docs"}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := m.call(t, fmt.Sprintf(`{"protocol":%q,"instance":"recall","deadlineMs":20000,%s,%s}`,
				sidecarProtocol, remote, params))
			failure, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("a remote-bound surface was answered from this machine's index: %s", mustJSON(resp))
			}
			if failure["code"] != "unavailable" {
				t.Errorf("code = %v, want unavailable", failure["code"])
			}
			if message, _ := failure["message"].(string); !strings.Contains(message, "workshop") {
				t.Errorf("message %q does not name the host it cannot answer for", message)
			}
		})
	}
}

// TestSidecarPluginSpendsTheDeadlineRatherThanIgnoringIt. deadlineMs is
// advisory but accurate: the host kills the process group when it expires and
// reports a crashed plugin. A budget too small to ask anything has to come
// back inside it, and what comes back must not claim the coverage it did not
// have — an empty page under `answered` is "the corpus holds nothing", which a
// query that never ran cannot say.
func TestSidecarPluginSpendsTheDeadlineRatherThanIgnoringIt(t *testing.T) {
	m := newPluginMachine(t)

	start := time.Now()
	resp := m.call(t, `{"protocol":"`+sidecarProtocol+`","method":"list","instance":"recall","deadlineMs":1,`+
		`"params":{"collection":"results","query":"corroboration","limit":20}}`)
	elapsed := time.Since(start)

	// The protocol's own list timeout is 10 s; anything at or past it would
	// have been killed rather than answered.
	if elapsed > 10*time.Second {
		t.Fatalf("a 1 ms budget took %s, which the host would have killed", elapsed)
	}
	if failure, ok := resp["error"].(map[string]any); ok {
		if failure["code"] != "unavailable" {
			t.Errorf("code = %v, want unavailable for a budget that bought nothing", failure["code"])
		}
		return
	}
	page := mustPage(t, resp)
	if items, _ := page["items"].([]any); len(items) == 0 && page["outcome"] == "answered" {
		t.Errorf("an empty page from a 1 ms budget claims the corpus holds nothing: %s", mustJSON(resp))
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

// sidecarTranscript is the second corpus: one file, a different store, and the
// same query term as the fixture corpus, so a profile or source change is
// observable as a change in which rows come back rather than as an empty page.
const sidecarTranscript = `# Standup transcript 2026-08-14

Marcus: ranking needs corroboration from a second source before we promote it.
Clara: agreed — one source saying it twice is not corroboration.
`

// sidecarFilterTOML is an installation with something to choose between: two
// profiles, two sources over two stores, and two declared record types. Every
// chooser recall declares is read from configuration, so this is what the
// filter tests are asserting against.
const sidecarFilterTOML = `
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
record_types = ["document"]

[[sources]]
source_uid = "01UIDCHAT"
source_id = "transcripts"
adapter = "documents"
location = "TRANSCRIPTS"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0
record_types = ["message"]

[profiles.work]
sources = ["docs", "transcripts"]
max_sensitivity = "internal"

[profiles.standups]
sources = ["transcripts"]
max_sensitivity = "internal"
`

// sidecarCeilingTOML is an installation whose second source sits above the
// default profile's sensitivity ceiling. It is the shape the `get` filters
// exist for: `journal` is searchable and expandable under `personal` and under
// nothing else, so a row found there and expanded under the pinned profile is
// denied.
const sidecarCeilingTOML = `
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

[[sources]]
source_uid = "01UIDCHAT"
source_id = "journal"
adapter = "documents"
location = "TRANSCRIPTS"
freshness_mode = "indexed"
sensitivity = "confidential"
base_prior = 1.0

[profiles.work]
sources = ["docs"]
max_sensitivity = "internal"

[profiles.personal]
sources = ["docs", "journal"]
max_sensitivity = "confidential"
`

// sidecarUnhealthyTOML configures one source that cannot be read. It is how the
// sources collection is asked the question the outcome rule exists for: rows
// that are all present, describing something that is not well.
const sidecarUnhealthyTOML = `
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

[[sources]]
source_uid = "01UIDGONE"
source_id = "vanished"
adapter = "documents"
location = "MISSING"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs", "vanished"]
max_sensitivity = "internal"
`

// assertNoControlRunes walks every string in a decoded response and fails on a
// control character. C0, DEL, and C1 are all of them except newline and tab,
// which are the only two with a meaning in a document body: an escape
// introduces a sequence, and the rest are invisible to a reader and
// meaningful to a terminal.
func assertNoControlRunes(t *testing.T, what string, value any) {
	t.Helper()
	switch v := value.(type) {
	case string:
		for _, r := range v {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Errorf("%s carries control rune %#U in %q", what, r, v)
				return
			}
		}
	case map[string]any:
		for key, entry := range v {
			assertNoControlRunes(t, what+"."+key, entry)
		}
	case []any:
		for i, entry := range v {
			assertNoControlRunes(t, fmt.Sprintf("%s[%d]", what, i), entry)
		}
	}
}

func itemIDs(t *testing.T, page map[string]any) []string {
	t.Helper()
	items, _ := page["items"].([]any)
	out := make([]string, 0, len(items))
	for _, entry := range items {
		row, _ := entry.(map[string]any)
		id, _ := row["id"].(string)
		if id == "" {
			t.Errorf("a row carries no id, so it could be neither opened nor acted on: %v", row)
		}
		out = append(out, id)
	}
	return out
}

func filtersOf(t *testing.T, collection map[string]any) []map[string]any {
	t.Helper()
	raw, _ := collection["filters"].([]any)
	if len(raw) == 0 {
		t.Fatalf("the collection declares no filters: %s", mustJSON(collection))
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		filter, _ := entry.(map[string]any)
		out = append(out, filter)
	}
	return out
}

func filterIDs(filters []map[string]any) []string {
	out := make([]string, 0, len(filters))
	for _, filter := range filters {
		id, _ := filter["id"].(string)
		out = append(out, id)
	}
	return out
}

func choicesOf(filter map[string]any) []map[string]any {
	raw, _ := filter["choices"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		choice, _ := entry.(map[string]any)
		out = append(out, choice)
	}
	return out
}

func choiceIDs(filter map[string]any) []string {
	choices := choicesOf(filter)
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		id, _ := choice["id"].(string)
		out = append(out, id)
	}
	return out
}

func coverageRows(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw, _ := page["coverage"].([]any)
	if len(raw) == 0 {
		t.Fatalf("the page carries no coverage table: %s", mustJSON(page))
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		row, _ := entry.(map[string]any)
		out = append(out, row)
	}
	return out
}

func coverageBySource(t *testing.T, page map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, row := range coverageRows(t, page) {
		source, _ := row["source"].(string)
		out[source] = row
	}
	return out
}

// rowSources is the set of sources a page's rows came from, sorted and deduped:
// what a scope filter changes is which sources answered, not how many rows each
// of them happened to contribute.
func rowSources(page map[string]any) []string {
	items, _ := page["items"].([]any)
	seen := map[string]bool{}
	var out []string
	for _, entry := range items {
		row, _ := entry.(map[string]any)
		cells, _ := row["cells"].(map[string]any)
		source, _ := cells["source"].(string)
		if source == "" || seen[source] {
			continue
		}
		seen[source] = true
		out = append(out, source)
	}
	sort.Strings(out)
	return out
}

func notices(page map[string]any) []string {
	raw, _ := page["notices"].([]any)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		notice, _ := entry.(map[string]any)
		text, _ := notice["text"].(string)
		out = append(out, text)
	}
	return out
}

// Two of this surface's rules cannot be reached from a real corpus. Nothing
// recall ships panics on purpose, and every source failing is not the same as
// no source being configured: a documents source with a missing location is
// SKIPPED, which is degraded coverage, not a failed query. Both are reached by
// running the real command in a child process over a scripted core — a real
// process, real streams, and a real exit code, which is the whole reason this
// file tests through a process at all.
const pluginScriptEnv = "RECALL_TEST_PLUGIN_SCRIPT"

// scriptedCore is a recall core with one behaviour written into it.
type scriptedCore struct{ script string }

func (c *scriptedCore) Query(context.Context, recall.QueryRequest) (recall.QueryResponse, error) {
	switch c.script {
	case "panic":
		panic(scriptedPanic)
	case "failed":
		// Every source that was asked failed: an empty list here would claim
		// the corpus holds nothing, and nothing looked.
		return recall.QueryResponse{
			Outcome:  recall.OutcomeFailed,
			Coverage: recall.CoverageDegraded,
			SourceOutcomes: []recall.SourceReport{{
				SourceID: "docs", Outcome: recall.SearchFailed, Reason: "unreachable",
			}},
		}, nil
	case "ledger":
		// One response carrying every part of the ledger the page projects:
		// records withheld, results a budget removed, and a source in each of
		// recall's search outcomes. No real corpus produces all of them at
		// once, and the mapping between the two vocabularies is exactly what
		// has to stay pinned.
		return recall.QueryResponse{
			Outcome:        recall.OutcomeAnswered,
			Coverage:       recall.CoverageDegraded,
			DroppedResults: 6,
			Suppressed: []recall.Suppression{
				{Reason: "below_relevance_floor", Count: 1},
				{Reason: "duplicate_view", Count: 2},
			},
			SourceOutcomes: []recall.SourceReport{
				{SourceID: "docs", Outcome: recall.SearchSuccess, Elapsed: 12 * time.Millisecond},
				{SourceID: "slow", Outcome: recall.SearchTimeout, Reason: "budget_exhausted",
					Elapsed: 2000 * time.Millisecond},
				{SourceID: "mail", Outcome: recall.SearchUnavailable, Reason: "unreachable"},
				{SourceID: "vault", Outcome: recall.SearchDenied, Reason: "denied"},
				{SourceID: "half", Outcome: recall.SearchPartial},
				{SourceID: "other", Outcome: recall.SearchSkipped, Reason: "out_of_profile"},
				{SourceID: "crashed", Outcome: recall.SearchFailed, Reason: "panicked"},
			},
		}, nil
	}
	return recall.QueryResponse{}, nil
}

func (c *scriptedCore) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	if c.script == "panic" {
		panic(scriptedPanic)
	}
	return recall.ExpandResponse{}, nil
}

func (c *scriptedCore) Refresh(context.Context, recall.RefreshRequest) (recall.RefreshResponse, error) {
	if c.script == "panic" {
		panic(scriptedPanic)
	}
	return recall.RefreshResponse{}, nil
}

func (c *scriptedCore) Sources(context.Context) (api.Listing, error) {
	if c.script == "panic" {
		panic(scriptedPanic)
	}
	return api.Listing{}, nil
}

func (c *scriptedCore) Doctor(context.Context) (api.Listing, error) {
	return api.Listing{}, nil
}

func (c *scriptedCore) Profile() string { return "" }

const scriptedPanic = "scripted crash: nil map read inside a source adapter"

// TestSidecarPluginScriptedCoreChild is the child half of the scripted tests.
// It is skipped in an ordinary run.
func TestSidecarPluginScriptedCoreChild(t *testing.T) {
	script := os.Getenv(pluginScriptEnv)
	if script == "" {
		t.Skip("child half of the scripted-core plugin tests")
	}
	core := api.Core(&scriptedCore{script: script})
	if script == fanOutPanicScript {
		core = fanOutPanicCore(t)
	}
	os.Exit(cli.Run(context.Background(), cli.Env{
		Args:   []string{"sidecar-plugin"},
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Core:   core,
	}))
}

// The scripted panics above all happen on the request's own goroutine, where
// one recover at the top of the command can catch them. The panic this guards
// against does not: refresh fans out over sources, and an adapter's Initialize,
// Refresh, and Health all run on a goroutine no caller can recover on. Deleting
// the per-source recovery would leave every test above passing and this one
// killing the process.
//
// It is run over a real recall core rather than a scripted one for the same
// reason the rest of this file runs a real process: the goroutine has to be the
// one recall actually starts.
const (
	fanOutPanicScript = "refresh-fan-out-panic"
	fanOutPanicDetail = "scripted crash: nil index inside a source adapter's refresh"
)

// fanOutPanicCore is a real recall core over one configured source whose
// adapter crashes when it is refreshed.
func fanOutPanicCore(t *testing.T) api.Core {
	t.Helper()

	home := filepath.Join(os.Getenv("HOME"), "core")
	configHome := filepath.Join(home, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "recall"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configHome, "recall", "config.toml"),
		[]byte(fanOutPanicConfig), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{
		Paths: config.Paths{
			ConfigHome: configHome,
			StateHome:  filepath.Join(home, "state"),
			CacheHome:  filepath.Join(home, "cache"),
		},
		Builtins: []config.Builtin{{
			Name:           "crashing",
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		}},
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	registry := source.NewRegistry(cfg, source.Options{
		Builtins: map[string]source.Factory{
			"crashing": func() adapter.Adapter { return crashingAdapter{} },
		},
		StateDir: filepath.Join(home, "state"),
	})
	ranker, err := ranking.New(app.RankingConfig(cfg, 0))
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	return &appCore{app: app.New(app.Options{Config: cfg, Registry: registry, Ranker: ranker})}
}

const fanOutPanicConfig = `
[defaults]
profile = "default"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDCRASH"
source_id = "docs"
adapter = "crashing"
location = "/tmp/crashing"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.default]
sources = ["docs"]
max_sensitivity = "internal"
`

// crashingAdapter handshakes successfully — a source that failed to initialize
// would never reach the refresh the test is about — and then crashes on the
// fan-out goroutine.
type crashingAdapter struct{}

func (crashingAdapter) Initialize(context.Context, adapter.Config) (recall.Manifest, error) {
	return recall.Manifest{
		ProtocolVersion: 1,
		AdapterID:       "crashing/1",
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessIndexed},
		AsOfSupport:     recall.AsOfFilter,
		RelevanceBasis:  recall.RelevanceLexicalSpan,
		Capabilities: []recall.Capability{
			recall.CapSearch, recall.CapExpand, recall.CapCheckpoint,
		},
	}, nil
}

func (crashingAdapter) Search(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
	panic(fanOutPanicDetail)
}

func (crashingAdapter) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	panic(fanOutPanicDetail)
}

func (crashingAdapter) Health(context.Context) (recall.Health, error) {
	panic(fanOutPanicDetail)
}

func (crashingAdapter) Refresh(context.Context, protocol.RefreshParams) (recall.Health, error) {
	panic(fanOutPanicDetail)
}

func (crashingAdapter) Close() error { return nil }

// appCore is the same delegation the CLI's own surface core performs, without
// the runtime that builds the compiled-in adapters. Only Refresh is exercised.
type appCore struct{ app *app.App }

func (c *appCore) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	return c.app.Query(ctx, req)
}

func (c *appCore) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	return c.app.Expand(ctx, req, "")
}

func (c *appCore) Refresh(ctx context.Context, req recall.RefreshRequest) (recall.RefreshResponse, error) {
	return c.app.Refresh(ctx, req)
}

func (c *appCore) Sources(context.Context) (api.Listing, error) { return api.Listing{}, nil }
func (c *appCore) Doctor(context.Context) (api.Listing, error)  { return api.Listing{}, nil }
func (c *appCore) Profile() string                              { return "" }

// callScripted runs one plugin invocation in a child process over a scripted
// core and returns the decoded response and what reached standard error. The
// transport rules are enforced here exactly as they are for a real corpus.
func callScripted(t *testing.T, script, request string) (map[string]any, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSidecarPluginScriptedCoreChild$") //nolint:gosec // the test binary itself
	home := t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"SIDECAR_PLUGIN=1",
		pluginScriptEnv + "=" + script,
	}
	cmd.Stdin = strings.NewReader(request)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("exit was not 0; the host reads that as a transport failure and tells the user "+
			"recall crashed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("stdout was not one JSON object: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout carried more than one value\nstdout: %s", stdout.String())
	}
	if got := resp["protocol"]; got != sidecarProtocol {
		t.Fatalf("response protocol = %v, want %q", got, sidecarProtocol)
	}
	return resp, stderr.String()
}

// A panic is the one failure the plugin cannot report by returning: unrecovered
// it exits non-zero with a goroutine dump and NO response, which the protocol
// reads as a transport failure attributed to the plugin. The user is then told
// recall crashed instead of being shown the internal error it actually is.
func TestSidecarPluginAnswersAPanicWithOneTypedInternalError(t *testing.T) {
	for _, tc := range []struct{ name, request string }{
		{"list", sidecarRequest("list", `{"collection":"results","query":"anything"}`)},
		{"get", sidecarRequest("get", `{"collection":"results","id":"docs:notes.md"}`)},
		{"listSources", sidecarRequest("list", `{"collection":"sources"}`)},
		{"act", sidecarRequest("act", `{"action":"refresh-source","collection":"sources","id":"docs"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, stderr := callScripted(t, "panic", tc.request)

			fail, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("a panic answered with %v, want a typed error", resp)
			}
			if fail["code"] != "internal" {
				t.Errorf("code = %v, want internal", fail["code"])
			}
			if fail["retryable"] == true {
				t.Error("retryable = true; a crash repeats until the bug is fixed")
			}
			if msg, _ := fail["message"].(string); msg == "" {
				t.Error("a typed failure with no message tells the user nothing")
			}
			for _, page := range []string{"page", "resource", "outcome", "plugin"} {
				if _, present := resp[page]; present {
					t.Errorf("the response carries %q as well as an error; exactly one is allowed", page)
				}
			}

			// The crash detail belongs on stderr, which the host drains, and
			// nowhere near the one value stdout may carry.
			if !strings.Contains(stderr, scriptedPanic) {
				t.Errorf("stderr does not name the panic, so the bug is not findable:\n%s", stderr)
			}
			if !strings.Contains(stderr, "goroutine ") {
				t.Errorf("stderr carries no stack:\n%s", stderr)
			}
			body := mustJSON(resp)
			if strings.Contains(body, scriptedPanic) || strings.Contains(body, "goroutine ") {
				t.Errorf("the panic text or its stack reached stdout: %s", body)
			}
		})
	}
}

// The same rule for the crash that recover at the request boundary cannot
// catch. refresh-source fans out over sources, and an adapter that panics does
// it on a goroutine of recall's own. Unrecovered there the process dies with a
// goroutine dump and no response, and the host tells the user recall crashed.
//
// The answer is one JSON object, exit 0, and a typed failure that names the
// source — with the crash detail and its stack on standard error, where the
// host drains them and where they cannot corrupt the one value stdout carries.
func TestSidecarPluginAnswersAPanicOnTheRefreshFanOutWithOneTypedFailure(t *testing.T) {
	resp, stderr := callScripted(t, fanOutPanicScript,
		sidecarRequest("act", `{"action":"refresh-source","collection":"sources","id":"docs"}`))

	if fail, ok := resp["error"].(map[string]any); ok {
		// A typed error is a legitimate shape for a failed act, but this one
		// must not be the internal error the request-boundary recover produces:
		// that would mean the panic escaped the fan-out.
		if fail["code"] == "internal" {
			t.Fatalf("the crash reached the request boundary, so the fan-out did not "+
				"recover it: %v", fail)
		}
		return
	}
	outcome, ok := resp["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("a crashed source answered with %v, want a typed outcome", resp)
	}
	if outcome["status"] != "failed" {
		t.Errorf("status = %v, want failed: the source produced no usable state",
			outcome["status"])
	}
	msg, _ := outcome["message"].(string)
	if !strings.Contains(msg, "docs") || !strings.Contains(msg, string(recall.RefreshPanicked)) {
		t.Errorf("message = %q, want the source named and its reason stated", msg)
	}

	if !strings.Contains(stderr, fanOutPanicDetail) {
		t.Errorf("stderr does not name the panic, so the bug is not findable:\n%s", stderr)
	}
	if !strings.Contains(stderr, "goroutine ") {
		t.Errorf("stderr carries no stack:\n%s", stderr)
	}
	body := mustJSON(resp)
	if strings.Contains(body, fanOutPanicDetail) || strings.Contains(body, "goroutine ") {
		t.Errorf("the panic text or its stack reached stdout: %s", body)
	}
}

// Every source recall asked failed, so an empty list would claim the corpus
// holds nothing when in fact nothing looked. That is the `failed` outcome: a
// page, because it is a statement about this row set, and the host draws an
// error card over it rather than "no matches". It is asserted through a
// scripted core because no real configuration reaches it — an unreachable
// documents source is SKIPPED, which is degraded, not failed.
func TestSidecarPluginReportsAnEverySourceFailedQueryAsAFailedPage(t *testing.T) {
	resp, _ := callScripted(t, "failed",
		sidecarRequest("list", `{"collection":"results","query":"anything"}`))

	if _, present := resp["error"]; present {
		t.Fatalf("every source failing answered with a typed error; the outcome carries it now: %s",
			mustJSON(resp))
	}
	page := mustPage(t, resp)
	if page["outcome"] != "failed" {
		t.Fatalf("outcome = %v, want failed: %s", page["outcome"], mustJSON(resp))
	}
	if items, _ := page["items"].([]any); len(items) != 0 {
		t.Errorf("a failed page carried rows: %s", mustJSON(resp))
	}
	named := false
	for _, text := range notices(page) {
		if strings.Contains(text, "docs") {
			named = true
		}
	}
	if !named {
		t.Errorf("no notice names the source that failed: %s", mustJSON(page))
	}
	rows := coverageRows(t, page)
	if len(rows) != 1 || rows[0]["source"] != "docs" || rows[0]["state"] != "failed" {
		t.Errorf("coverage = %s, want one failed row for docs", mustJSON(page["coverage"]))
	}
	if rows[0]["reason"] != "unreachable" {
		t.Errorf("coverage reason = %v, want the reason recall reported", rows[0]["reason"])
	}
}

// Recall is global, whatever project the surface happens to be showing.
//
// It used to map context.project.name onto recall's Scope.Project, which a
// documents source reads as the first path segment under its own root: a
// surface showing a project therefore asked every documents source for records
// filed under a folder of that name, matched none, and the adapter answered
// success-with-no-candidates — an empty page the host drew as "no matches,
// sources fine". This is td-35bcd1's proof case: a project name nothing on this
// machine has must answer exactly what no context at all answers.
func TestSidecarPluginAnswersGloballyWhateverProjectIsOnScreen(t *testing.T) {
	m := newPluginMachine(t)
	query := `"method":"list","params":{"collection":"results","query":"corroboration","limit":20}`

	global := m.call(t, fmt.Sprintf(`{"protocol":%q,"instance":"recall","deadlineMs":20000,%s}`,
		sidecarProtocol, query))
	scoped := m.call(t, fmt.Sprintf(
		`{"protocol":%q,"instance":"recall","deadlineMs":20000,`+
			`"context":{"project":{"root":"/checkout","workDir":"/checkout",`+
			`"name":"nosuchproject","branch":"main"}},%s}`,
		sidecarProtocol, query))

	globalPage, scopedPage := mustPage(t, global), mustPage(t, scoped)
	if ids := itemIDs(t, globalPage); len(ids) == 0 {
		t.Fatalf("no rows for a term the fixture corpus holds: %s", mustJSON(global))
	}
	if got, want := itemIDs(t, scopedPage), itemIDs(t, globalPage); !jsonEqual(got, want) {
		t.Errorf("a project on screen changed the answer\n with context: %v\nwithout it:  %v", got, want)
	}
	if got, want := scopedPage["outcome"], globalPage["outcome"]; got != want {
		t.Errorf("outcome = %v with a project on screen, %v without it", got, want)
	}
	for _, text := range notices(scopedPage) {
		if strings.Contains(text, "nosuchproject") || strings.Contains(text, "scoped to project") {
			t.Errorf("a project scope was applied and announced: %q", text)
		}
	}
}

// deadlineMs is milliseconds and time.Duration is nanoseconds, so a large
// enough deadline used to multiply past int64 and come back as an instant
// already gone — a bigger budget answering less than a small one.
func TestSidecarPluginClampsAnAbsurdDeadlineInsteadOfOverflowing(t *testing.T) {
	m := newPluginMachine(t)
	resp := m.call(t, fmt.Sprintf(
		`{"protocol":%q,"method":"list","instance":"recall","deadlineMs":9223372036854775807,`+
			`"params":{"collection":"results","query":"corroboration","limit":20}}`, sidecarProtocol))

	page := mustPage(t, resp)
	if items, _ := page["items"].([]any); len(items) == 0 {
		t.Fatalf("the largest possible budget answered nothing: %s", mustJSON(resp))
	}
}

// Every chooser recall declares is configuration read back. A filter offering
// something the query would then refuse — a profile this machine does not have,
// a source it never configured — is a control that exists only to fail.
func TestSidecarPluginDeclaresItsFiltersFromConfiguration(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarFilterTOML, false, nil)
	resp := m.call(t, sidecarRequest("describe", ""))

	filters := filtersOf(t, collectionsByID(t, resp)["results"])
	if got := filterIDs(filters); !equalStrings(got, []string{"profile", "source", "type", "since"}) {
		t.Fatalf("filters = %v, want profile, source, type, since in that order", got)
	}
	// The first declared filter is the collection's scope: its title is what
	// the host folds into the pill, so it has to be the one that decides which
	// sources are asked at all.
	if filters[0]["id"] != "profile" {
		t.Errorf("scope filter = %v, want profile first", filters[0]["id"])
	}

	profile := filters[0]
	if profile["kind"] != "choice" {
		t.Errorf("profile kind = %v, want choice", profile["kind"])
	}
	if got := choiceIDs(profile); !equalStrings(got, []string{"standups", "work"}) {
		t.Errorf("profile choices = %v, want every configured profile", got)
	}
	if profile["default"] != "work" {
		t.Errorf("profile default = %v, want the configured default profile", profile["default"])
	}
	if got := choiceIDs(filters[1]); !equalStrings(got, []string{"any", "docs", "transcripts"}) {
		t.Errorf("source choices = %v, want Any plus every configured source", got)
	}
	if filters[1]["default"] != "any" {
		t.Errorf("source default = %v, want any", filters[1]["default"])
	}
	if got := choiceIDs(filters[2]); !equalStrings(got, []string{"any", "document", "message"}) {
		t.Errorf("type choices = %v, want Any plus the record types configuration declares", got)
	}
	since := filters[3]
	if since["kind"] != "text" {
		t.Errorf("since kind = %v, want text", since["kind"])
	}
	if raw, present := since["choices"]; present {
		if list, _ := raw.([]any); len(list) > 0 {
			t.Errorf("since carries choices %v; a text filter has none", raw)
		}
	}
	for _, filter := range filters {
		id, _ := filter["id"].(string)
		if len(id) > 32 {
			t.Errorf("filter id %q is longer than the protocol's 32 characters", id)
		}
		for _, choice := range choicesOf(filter) {
			for _, key := range []string{"id", "title"} {
				if value, _ := choice[key].(string); len([]rune(value)) > 32 {
					t.Errorf("choice %s %q is longer than the protocol's 32 characters", key, value)
				}
			}
		}
		if n := len(choicesOf(filter)); n > 64 {
			t.Errorf("filter %q declares %d choices; the protocol's bound is 64", id, n)
		}
	}
	if n := len(filters); n > 8 {
		t.Errorf("results declares %d filters; the protocol's bound is 8", n)
	}
}

// An installation that pins the plugin with `--profile NAME` declares THAT
// profile as the scope filter's default, not the configured one.
//
// The host does not send a filter whose value equals its default, so the
// default is not decoration: it is the name of the profile a page with no
// filters was gathered under. Declaring the configured default while the flag
// pinned another puts a scope in the pill no page ever ran under, and makes the
// pinned profile the one choice in the radio group that cannot be selected —
// choosing it sends nothing, and nothing means the flag's profile again.
func TestSidecarPluginDeclaresThePinnedProfileAsTheScopeDefault(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarFilterTOML, false, nil)

	described := m.callPinned(t, "standups", sidecarRequest("describe", ""))
	profile := filtersOf(t, collectionsByID(t, described)["results"])[0]
	if profile["default"] != "standups" {
		t.Errorf("profile default = %v under --profile standups, want the profile the plugin is pinned to",
			profile["default"])
	}

	// And the declaration is true: an unfiltered list runs under exactly it.
	page := mustPage(t, m.callPinned(t, "standups", sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20}`)))
	if got := rowSources(page); !jsonEqual(got, []string{"transcripts"}) {
		t.Errorf("a pinned installation answered from %v, want the pinned profile's own source: %s",
			got, mustJSON(page))
	}

	// With no pin, the configured default is what both halves mean.
	unpinned := m.call(t, sidecarRequest("describe", ""))
	if got := filtersOf(t, collectionsByID(t, unpinned)["results"])[0]["default"]; got != "work" {
		t.Errorf("profile default = %v with no pin, want the configured default profile", got)
	}
}

// A list with no filters resolves the configured default profile, and a filter
// changes which sources answer. This is the whole of the scope decision: what a
// page covers is chosen by the user and visible in the pill, never applied
// behind their back.
func TestSidecarPluginResolvesTheDefaultProfileAndSwitchesOnTheFilter(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarFilterTOML, false, nil)

	unfiltered := mustPage(t, m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20}`)))
	if got := rowSources(unfiltered); !jsonEqual(got, []string{"docs", "transcripts"}) {
		t.Fatalf("the default profile answered from %v, want both of its sources", got)
	}

	narrowed := mustPage(t, m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20,`+
			`"filters":{"profile":"standups"}}`)))
	if got := rowSources(narrowed); !jsonEqual(got, []string{"transcripts"}) {
		t.Fatalf("the standups profile answered from %v, want only its own source: %s",
			got, mustJSON(narrowed))
	}
	if narrowed["outcome"] != "answered" {
		t.Errorf("outcome = %v, want answered: the chosen profile's sources all answered",
			narrowed["outcome"])
	}

	// And the same narrowing by source, inside the profile that has it.
	bySource := mustPage(t, m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20,`+
			`"filters":{"source":"transcripts"}}`)))
	if got := rowSources(bySource); !jsonEqual(got, []string{"transcripts"}) {
		t.Errorf("source=transcripts answered from %v", got)
	}
}

// A row expands under the profile it was found in.
//
// `get` carries the filters the list that produced the row was showing, and
// resolves them through the same code list does. Without them the expansion ran
// under the pinned profile, whose ceiling the row's source is above, and the
// host showed a denial for a row it had just drawn. The denial is still there
// and still correct — it is a permission recheck, not a formality — it is
// simply asked about the profile the caller chose.
func TestSidecarPluginExpandsUnderTheProfileTheRowWasFoundIn(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarCeilingTOML, false, nil)
	const filters = `"filters":{"profile":"personal"}`

	// The row is only reachable under the raised ceiling, which is what makes
	// the rest of this test a statement about scope rather than about luck.
	page := mustPage(t, m.callPinned(t, "work", sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20,`+filters+`}`)))
	id := rowIDForSource(t, page, "journal")

	answered := m.callPinned(t, "work", sidecarRequest("get",
		fmt.Sprintf(`{"collection":"results","id":%q,%s}`, id, filters)))
	doc, ok := answered["resource"].(map[string]any)
	if !ok {
		t.Fatalf("a row found under profile=personal did not expand under it: %s", mustJSON(answered))
	}
	if doc["identity"] != id {
		t.Errorf("identity = %v, want the row id %q back", doc["identity"], id)
	}
	if body := sectionBody(doc, "Evidence"); !strings.Contains(body, "orroboration") {
		t.Errorf("the Evidence section does not carry the record's text: %s", mustJSON(doc))
	}

	// And the same locator without the filters is the denial the pinned
	// profile owes: recall never widens a ceiling on its own.
	denied := m.callPinned(t, "work", sidecarRequest("get",
		fmt.Sprintf(`{"collection":"results","id":%q}`, id)))
	failure, ok := denied["error"].(map[string]any)
	if !ok {
		t.Fatalf("the pinned profile expanded a source above its ceiling: %s", mustJSON(denied))
	}
	message, _ := failure["message"].(string)
	for _, want := range []string{"journal", "work", "internal"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not name %q", message, want)
		}
	}

	// A filter value that is not configured is refused on get exactly as it is
	// on list: a filter declared and then ignored answers a question nobody
	// asked.
	unknown := m.call(t, sidecarRequest("get",
		fmt.Sprintf(`{"collection":"results","id":%q,"filters":{"profile":"nope"}}`, id)))
	if _, ok := unknown["error"].(map[string]any); !ok {
		t.Errorf("get accepted a profile this machine does not declare: %s", mustJSON(unknown))
	}
}

// rowIDForSource is the id of the first row a given source contributed.
func rowIDForSource(t *testing.T, page map[string]any, source string) string {
	t.Helper()
	items, _ := page["items"].([]any)
	for _, entry := range items {
		row, _ := entry.(map[string]any)
		cells, _ := row["cells"].(map[string]any)
		if cells["source"] != source {
			continue
		}
		if id, _ := row["id"].(string); id != "" {
			return id
		}
	}
	t.Fatalf("no row from source %q: %s", source, mustJSON(page))
	return ""
}

// A source outside the chosen profile is recall's own refusal, verbatim: it
// names a profile that does have the source, which is the sentence that makes
// the next attempt a real one rather than a guess. The alternative — an empty
// page under `abstained` — would say every eligible source answered and none
// knew, over a request where nothing was asked.
func TestSidecarPluginRefusesASourceOutsideTheChosenProfile(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarFilterTOML, false, nil)
	resp := m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20,`+
			`"filters":{"profile":"standups","source":"docs"}}`))

	failure, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("a source outside the profile was answered: %s", mustJSON(resp))
	}
	if failure["code"] != "invalid_request" {
		t.Errorf("code = %v, want invalid_request: it is the caller's to fix", failure["code"])
	}
	if failure["retryable"] == true {
		t.Error("retryable = true; repeating this unchanged fails the same way")
	}
	message, _ := failure["message"].(string)
	for _, want := range []string{"docs", "standups", "work"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not name %q", message, want)
		}
	}
}

// The two filters that narrow what an answering source may return.
func TestSidecarPluginNarrowsByTypeAndSince(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarFilterTOML, false, nil)
	const query = `{"collection":"results","query":"corroboration","limit":20`

	// A record type decides which sources are eligible at all: one that
	// declares it does not hold the type is not asked, and the coverage table
	// says so rather than the page reporting an empty success over it.
	t.Run("type", func(t *testing.T) {
		documents := mustPage(t, m.call(t, sidecarRequest("list",
			query+`,"filters":{"type":"document"}}`)))
		if got := rowSources(documents); !jsonEqual(got, []string{"docs"}) {
			t.Errorf("type=document answered from %v, want the source that declares it: %s",
				got, mustJSON(documents))
		}
		if row := coverageBySource(t, documents)["transcripts"]; row["state"] != "skipped" {
			t.Errorf("transcripts coverage = %s, want skipped: it declares no document records",
				mustJSON(row))
		}

		messages := mustPage(t, m.call(t, sidecarRequest("list",
			query+`,"filters":{"type":"message"}}`)))
		row := coverageBySource(t, messages)["docs"]
		if row["state"] != "skipped" {
			t.Errorf("docs coverage = %s, want skipped: it declares no message records", mustJSON(row))
		}
		if reason, _ := row["reason"].(string); !strings.Contains(reason, "record_type") {
			t.Errorf("reason = %q, want the record-type mismatch recall reported", reason)
		}
	})

	t.Run("since keeps what is newer", func(t *testing.T) {
		page := mustPage(t, m.call(t, sidecarRequest("list",
			query+`,"filters":{"since":"2000-01-01"}}`)))
		if got := rowSources(page); len(got) == 0 {
			t.Errorf("a date every record is newer than removed every row: %s", mustJSON(page))
		}
	})

	t.Run("since drops what is older", func(t *testing.T) {
		future := time.Now().AddDate(1, 0, 0).UTC().Format("2006-01-02")
		page := mustPage(t, m.call(t, sidecarRequest("list",
			query+fmt.Sprintf(`,"filters":{"since":%q}}`, future))))
		if items, _ := page["items"].([]any); len(items) != 0 {
			t.Errorf("a date in the future kept rows: %s", mustJSON(page))
		}
		// Nothing failed and nothing was withheld from a source that could not
		// answer: this is a true abstention over the filtered corpus.
		if page["outcome"] != "abstained" {
			t.Errorf("outcome = %v, want abstained", page["outcome"])
		}
	})

	t.Run("an unreadable date is refused by name", func(t *testing.T) {
		resp := m.call(t, sidecarRequest("list", query+`,"filters":{"since":"last tuesday"}}`))
		failure, ok := resp["error"].(map[string]any)
		if !ok {
			t.Fatalf("an unparseable date was applied or ignored: %s", mustJSON(resp))
		}
		if failure["code"] != "invalid_request" {
			t.Errorf("code = %v, want invalid_request", failure["code"])
		}
		message, _ := failure["message"].(string)
		if !strings.Contains(message, "since") || !strings.Contains(message, "RFC 3339") {
			t.Errorf("message %q does not name the filter and the form it wants", message)
		}
	})

	t.Run("a filter recall never declared is refused", func(t *testing.T) {
		resp := m.call(t, sidecarRequest("list", query+`,"filters":{"project":"clara-home"}}`))
		failure, ok := resp["error"].(map[string]any)
		if !ok {
			t.Fatalf("an undeclared filter was accepted: %s", mustJSON(resp))
		}
		if message, _ := failure["message"].(string); !strings.Contains(message, "project") {
			t.Errorf("message %q does not name the filter it does not have", message)
		}
	})
}

// What a page does not show, and what each source did. Both are data rather
// than a sentence, because the host renders one in the summary row and the
// other as a table, and a plugin writing either as prose would be choosing the
// layout for it.
func TestSidecarPluginReportsOmittedAndPerSourceCoverage(t *testing.T) {
	resp, _ := callScripted(t, "ledger",
		sidecarRequest("list", `{"collection":"results","query":"anything"}`))
	page := mustPage(t, resp)

	omitted, ok := page["omitted"].(map[string]any)
	if !ok {
		t.Fatalf("no omitted block over a response that withheld records and dropped results: %s",
			mustJSON(page))
	}
	if omitted["suppressed"] != float64(3) {
		t.Errorf("suppressed = %v, want every withheld record counted", omitted["suppressed"])
	}
	if omitted["dropped"] != float64(6) {
		t.Errorf("dropped = %v, want what the response budget removed", omitted["dropped"])
	}

	states := map[string]string{}
	reasons := map[string]string{}
	for _, row := range coverageRows(t, page) {
		source, _ := row["source"].(string)
		states[source], _ = row["state"].(string)
		reasons[source], _ = row["reason"].(string)
	}
	want := map[string]string{
		"docs":    "answered",
		"slow":    "timeout",
		"mail":    "unhealthy",
		"vault":   "unhealthy",
		"half":    "unhealthy",
		"other":   "skipped",
		"crashed": "failed",
	}
	if !jsonEqual(states, want) {
		t.Errorf("coverage states = %s\nwant %s", mustJSON(states), mustJSON(want))
	}
	if reasons["slow"] != "budget_exhausted" {
		t.Errorf("timeout reason = %q, want the reason recall reported", reasons["slow"])
	}
	// A partial search has no state of its own in the protocol, so it must at
	// least say what happened: an "answered" row would leave a degraded page
	// with nothing in the table explaining it.
	if !strings.Contains(reasons["half"], "partial") {
		t.Errorf("partial reason = %q, want it to say the boundary was not fully searched", reasons["half"])
	}
	for _, row := range coverageRows(t, page) {
		if n := len([]rune(fmt.Sprint(row["reason"]))); n > 200 {
			t.Errorf("coverage reason is %d runes; the protocol's bound is 200", n)
		}
	}
	if elapsed := coverageBySource(t, page)["slow"]["elapsedMs"]; elapsed != float64(2000) {
		t.Errorf("elapsedMs = %v, want the source's own duration in milliseconds", elapsed)
	}
	if _, present := coverageBySource(t, page)["mail"]["elapsedMs"]; present {
		t.Error("a source that reported no duration carries an elapsedMs of zero, which reads as instant")
	}
}

// A real page carries the same table, so the coverage modal is not a shape only
// a fixture produces.
func TestSidecarPluginCarriesCoverageOnARealPage(t *testing.T) {
	m := newPluginMachineWith(t, true)
	page := mustPage(t, m.call(t, sidecarRequest("list",
		`{"collection":"results","query":"corroboration","limit":20}`)))
	if page["outcome"] != "degraded" {
		t.Fatalf("outcome = %v, want degraded: %s", page["outcome"], mustJSON(page))
	}
	rows := coverageBySource(t, page)
	row, ok := rows["docs"]
	if !ok {
		t.Fatalf("no coverage row for the source that could not fully answer: %s", mustJSON(page))
	}
	if row["state"] == "answered" {
		t.Errorf("state = answered on the source that degraded the page: %s", mustJSON(row))
	}
}

// The outcome describes the row set of this page and nothing else. Every
// configured source is in the list, so the list is complete — what is unwell is
// what the rows describe, and the status pill on each row is where that lives.
func TestSidecarPluginAnswersTheSourcesCollectionEvenWhenASourceIsUnwell(t *testing.T) {
	m := newPluginMachineConfigured(t, sidecarUnhealthyTOML, false, nil)
	page := mustPage(t, m.call(t, sidecarRequest("list", `{"collection":"sources"}`)))

	if page["outcome"] != "answered" {
		t.Fatalf("outcome = %v, want answered: every configured source is in the list",
			page["outcome"])
	}
	items, _ := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("sources = %d rows, want both configured sources: %s", len(items), mustJSON(page))
	}
	unwell := false
	for _, entry := range items {
		row, _ := entry.(map[string]any)
		cells, _ := row["cells"].(map[string]any)
		if cells["name"] != "vanished" {
			continue
		}
		status, _ := row["status"].(map[string]any)
		if tone, _ := status["tone"].(string); tone == "danger" || tone == "warning" {
			unwell = true
		}
	}
	if !unwell {
		t.Errorf("the row for a source that cannot be read carries no unwell status: %s", mustJSON(page))
	}
	if len(notices(page)) == 0 {
		t.Error("a list containing a source that cannot answer says so in no notice")
	}
}

// JSON null is four bytes rather than none, so a params-less request that
// spells it out used to decode into a zero value and be refused for naming a
// collection it never named.
func TestSidecarPluginReadsNullParamsAsNoParams(t *testing.T) {
	m := newPluginMachine(t)
	resp := m.call(t, fmt.Sprintf(
		`{"protocol":%q,"method":"list","instance":"recall","deadlineMs":20000,"params":null}`,
		sidecarProtocol))

	failure, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("a request with no params was answered: %s", mustJSON(resp))
	}
	if failure["code"] != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", failure["code"])
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "no params") {
		t.Errorf("message = %q, want it to name the envelope rather than a collection", message)
	}
}
