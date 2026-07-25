package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
)

// The exit-code mapping is a contract a script depends on, so it is documented
// where a person will look for it: the top-level help and the help of every
// command whose exit code says something about the corpus.
func TestHelpDocumentsTheExitCodes(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	for _, args := range [][]string{
		{"help"},
		{"query", "--help"},
		{"expand", "--help"},
		{"sources", "--help"},
		{"doctor", "--help"},
	} {
		code, stdout, _ := h.run(args...)
		if code != cli.ExitOK {
			t.Errorf("%v exited %d, want 0", args, code)
		}
		for _, want := range []string{"answered", "abstained", "degraded", "failed"} {
			contains(t, stdout, want, "a script author must be able to read the mapping from --help")
		}
	}
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
		out  string
	}{
		{"version", []string{"version"}, cli.ExitOK, "recall "},
		{"version flag", []string{"--version"}, cli.ExitOK, "recall "},
		{"no arguments", nil, cli.ExitError, "usage:"},
		{"help", []string{"--help"}, cli.ExitOK, "commands:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})
			code, stdout, _ := h.run(tc.args...)
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if !strings.Contains(stdout, tc.out) {
				t.Errorf("output %q does not contain %q", stdout, tc.out)
			}
		})
	}
}

// Every command has to say why it could not run rather than exiting silently,
// and none of them may print a result when configuration did not load.
func TestUnloadableConfigurationFailsEveryCommand(t *testing.T) {
	for _, args := range [][]string{
		{"query", "anything"},
		{"expand", "docs:a.md"},
		{"sources"},
		{"config", "explain"},
	} {
		h := newHarness(t, harnessOptions{userTOML: "[[sources]]\nsource_id = 3\n"})
		code, stdout, stderr := h.run(args...)
		if code != cli.ExitError {
			t.Errorf("%v exited %d, want %d", args, code, cli.ExitError)
		}
		if stdout != "" {
			t.Errorf("%v printed output over an unloadable configuration:\n%s", args, stdout)
		}
		if stderr == "" {
			t.Errorf("%v failed silently", args)
		}
	}
}

// `recall query "text" --json` is the order most people type. The flag package
// stops at the first non-flag argument, so it used to become a second query
// term and a usage error — and a silently dropped --json would be worse still,
// since it changes the format a script is parsing.
func TestFlagsMayFollowPositionalArguments(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	beforeCode, beforeOut, _ := h.run("query", "--json", "anything")
	afterCode, afterOut, afterErr := h.run("query", "anything", "--json")

	if beforeCode != afterCode {
		t.Errorf("exit codes differ by flag position: %d vs %d (%s)", beforeCode, afterCode, afterErr)
	}
	if !json.Valid([]byte(afterOut)) {
		t.Errorf("--json after the query was not honored:\n%s", afterOut)
	}
	if beforeOut != afterOut {
		t.Error("flag position changed the output")
	}
}
