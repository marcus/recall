package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// The Gmail guide's profile block is meant to be pasted into config.toml.
// Keep it on the real strict-loader path so an invented or renamed key cannot
// sit in the documentation until a user discovers it through recall doctor.
func TestGmailGuideProfileExampleLoads(t *testing.T) {
	body, err := os.ReadFile("../../docs/gmail-adapter.md")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "Add `mail` to a profile"
	section := string(body)
	var ok bool
	_, section, ok = strings.Cut(section, marker)
	if !ok {
		t.Fatalf("gmail guide no longer contains %q", marker)
	}
	_, section, ok = strings.Cut(section, "```toml\n")
	if !ok {
		t.Fatal("gmail guide profile example has no TOML fence")
	}
	example, _, ok := strings.Cut(section, "\n```")
	if !ok {
		t.Fatal("gmail guide profile example has no closing fence")
	}

	// Supply the three sources named by the documented fragment. The fragment
	// itself stays byte-for-byte what a reader copies from the guide.
	home := writeHome(t, `
[[sources]]
source_uid = "01J8ZKQ4M7DOCS"
source_id = "docs"
adapter = "documents"
freshness_mode = "indexed"
sensitivity = "internal"

[[sources]]
source_uid = "01J8ZKQ4M8TASKS"
source_id = "tasks"
adapter = "documents"
freshness_mode = "indexed"
sensitivity = "internal"

[[sources]]
source_uid = "01J8ZKQ4M9MAIL"
source_id = "mail"
adapter = "documents"
freshness_mode = "indexed"
sensitivity = "confidential"

`+example)
	cfg, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if err != nil {
		t.Fatalf("load documented Gmail profile: %v", err)
	}
	profile, err := cfg.ActiveProfile("personal")
	if err != nil {
		t.Fatal(err)
	}
	if got := profile.MaxSensitivity.String(); got != "confidential" {
		t.Errorf("max_sensitivity = %q, want confidential", got)
	}
}
