package recall_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

func TestParseLocator(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantSrc   string
		wantLocal string
		wantErr   error
	}{
		{"simple", "tasks:td-f62256", "tasks", "td-f62256", nil},
		{"local keeps separators", "clara-docs:projects/spec.md#ranking:2", "clara-docs", "projects/spec.md#ranking:2", nil},
		{"no separator", "td-f62256", "", "", recall.ErrMalformedLocator},
		{"empty source", ":td-f62256", "", "", recall.ErrMalformedLocator},
		{"empty local", "tasks:", "", "", recall.ErrMalformedLocator},
		{"empty", "", "", "", recall.ErrMalformedLocator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := recall.ParseLocator(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.SourceID != tt.wantSrc || got.Local != tt.wantLocal {
				t.Errorf("got %+v, want source %q local %q", got, tt.wantSrc, tt.wantLocal)
			}
			if round := got.String(); round != tt.in {
				t.Errorf("round trip = %q, want %q", round, tt.in)
			}
		})
	}
}

// A rename of the display name must not move any persisted reference. This is
// the whole reason SourceUID exists, so it is asserted directly.
func TestRenameDoesNotMovePersistedIdentity(t *testing.T) {
	const uid = recall.SourceUID("01J8ZKQ4M7")

	before := recall.Locator{SourceID: "tasks", SourceUID: uid, Local: "td-f62256"}
	after := recall.Locator{SourceID: "work-items", SourceUID: uid, Local: "td-f62256"}

	beforeRoot, err := before.LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	afterRoot, err := after.LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if beforeRoot != afterRoot {
		t.Fatalf("rename moved lineage root: %q -> %q", beforeRoot, afterRoot)
	}
	if before.String() == after.String() {
		t.Fatal("display form should follow the rename")
	}
	if want := "01J8ZKQ4M7:td-f62256"; string(afterRoot) != want {
		t.Errorf("lineage root = %q, want %q", afterRoot, want)
	}
}

func TestPersistRequiresIdentity(t *testing.T) {
	unresolved := recall.Locator{SourceID: "tasks", Local: "td-f62256"}
	if _, err := unresolved.Persist(); !errors.Is(err, recall.ErrUnresolvedLocator) {
		t.Fatalf("err = %v, want ErrUnresolvedLocator", err)
	}
	if unresolved.Resolved() {
		t.Error("locator without a uid should not report resolved")
	}
}

func TestLineageRootRoundTrip(t *testing.T) {
	root := recall.LineageRoot("01J8ZKQ4M7:projects/spec.md#ranking")
	loc, err := root.Locator()
	if err != nil {
		t.Fatal(err)
	}
	if loc.SourceUID != "01J8ZKQ4M7" || loc.Local != "projects/spec.md#ranking" {
		t.Fatalf("got %+v", loc)
	}
	back, err := loc.LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if back != root {
		t.Errorf("round trip = %q, want %q", back, root)
	}
}

func TestLocatorJSONIsDisplayForm(t *testing.T) {
	loc := recall.Locator{SourceID: "tasks", SourceUID: "01J8ZKQ4M7", Local: "td-f62256"}
	b, err := json.Marshal(loc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"tasks:td-f62256"` {
		t.Fatalf("marshal = %s, want display form", b)
	}

	var got recall.Locator
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceID != "tasks" || got.Local != "td-f62256" {
		t.Fatalf("unmarshal = %+v", got)
	}
	// The immutable identity does not survive display form. Callers resolve it
	// against a profile; nothing may infer it.
	if got.SourceUID != "" {
		t.Errorf("unmarshal invented a source_uid: %q", got.SourceUID)
	}
}

// A locator naming no source would serialize to a bare local part that
// ParseLocator cannot read back. Emitting it would produce something that
// looks like a locator and is not one, so marshaling refuses.
func TestSourcelessLocatorRefusesToSerialize(t *testing.T) {
	_, err := json.Marshal(recall.Locator{Local: "td-f62256"})
	if !errors.Is(err, recall.ErrUnresolvedLocator) {
		t.Fatalf("err = %v, want ErrUnresolvedLocator", err)
	}
}
