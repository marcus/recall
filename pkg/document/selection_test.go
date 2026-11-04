package document_test

import (
	"reflect"
	"testing"

	"github.com/marcus/recall/pkg/document"
)

func TestSelection(t *testing.T) {
	s := document.Selection{ExcludeGlobs: []string{"**/mockups/**", "*.template.html"}, SiblingPreference: []string{".md", ".html"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	got, excluded := s.Select([]string{"a.html", "a.md", "only.html", "nested/report.htm", "mockups/a.md", "nested/mockups/a.md", "x.template.html", "nested/b.html", "nested/b.md"})
	want := []string{"a.md", "nested/b.md", "nested/report.htm", "only.html"}
	if !reflect.DeepEqual(got, want) || len(excluded) != 5 {
		t.Fatalf("got %v, excluded %v", got, excluded)
	}
	s.SiblingPreference = []string{".html", ".md"}
	got, _ = s.Select([]string{"a.md", "a.html"})
	if !reflect.DeepEqual(got, []string{"a.html"}) {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		p, rel string
		want   bool
	}{
		{"**/mockups/**", "mockups", true}, {"**/mockups/**", "a/mockups/x/y.html", true}, {"**/*.html", "x.html", true}, {"**/*.html", "a/x.html", true}, {"*.html", "a/x.html", false}, {"x?.[mh]*", "xy.md", true},
	} {
		if got := document.MatchGlob(tc.p, tc.rel); got != tc.want {
			t.Errorf("%s %s: %v", tc.p, tc.rel, got)
		}
	}
}

func TestSelectionValidation(t *testing.T) {
	for _, pattern := range []string{"[", "../x", "/x", "a//b", ""} {
		if err := (document.Selection{ExcludeGlobs: []string{pattern}}).Validate(); err == nil {
			t.Errorf("accepted %q", pattern)
		}
	}
	if err := (document.Selection{SiblingPreference: []string{"md"}}).Validate(); err == nil {
		t.Fatal("accepted invalid suffix")
	}
}
