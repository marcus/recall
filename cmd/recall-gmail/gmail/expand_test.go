package gmail

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

var expandedThread = threadPayload{
	Thread: struct {
		ID       string    `json:"id"`
		Messages []message `json:"messages"`
	}{
		ID: "thr_1",
		Messages: []message{
			{
				ID: "m1", LabelIDs: []string{"SENT"}, InternalDate: json.Number("1784900000000"),
				Headers: map[string]string{
					"date": "Fri, 24 Jul 2026 17:20:00 +0000",
					"from": "Marcus", "to": "Dana", "subject": "referral",
				},
				Body: "Could you send the referral letter?",
			},
			{
				ID: "m2", LabelIDs: []string{"INBOX"}, InternalDate: json.Number("1784995320000"),
				Headers: map[string]string{
					"date": "Sat, 25 Jul 2026 14:02:00 +0000",
					"from": "Dana", "to": "Marcus",
					"subject": "Re: referral portal.example/reset?token=secret",
				},
				Body: "Posted it. Upload at https://portal.example.test/a?t=abc",
			},
		},
	},
}

func expandAdapter(t *testing.T, runner *fakeRunner) *Adapter {
	t.Helper()
	return initialized(t, runner, nil)
}

func TestExpandDetailLevelsAndURLSafety(t *testing.T) {
	runner := &fakeRunner{answers: map[string]any{"gmail-thread-thr_1": expandedThread}}
	a := expandAdapter(t, runner)
	locator := recall.Locator{SourceID: "mail", Local: "thread/thr_1"}

	summary, err := a.Expand(t.Context(), recall.ExpandRequest{Locator: locator, Detail: recall.DetailSummary})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.Content, "Subject: Re: referral [url removed]") ||
		strings.Contains(summary.Content, "portal.example") ||
		strings.Contains(summary.Content, "Posted it") {
		t.Fatalf("summary = %q", summary.Content)
	}

	context, err := a.Expand(t.Context(), recall.ExpandRequest{Locator: locator, Detail: recall.DetailContext})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context.Content, "Could you send the referral letter?") ||
		!strings.Contains(context.Content, "[url removed]") ||
		strings.Contains(context.Content, "portal.example.test") {
		t.Fatalf("context = %q", context.Content)
	}
	call := runner.calls[len(runner.calls)-1]
	if !contains(call.Args, "--sanitize-content") || !contains(call.Args, "--full") {
		t.Fatalf("expand argv = %v", call.Args)
	}
}

func TestExpandHonorsByteBudgetAtRuneBoundary(t *testing.T) {
	runner := &fakeRunner{answers: map[string]any{"gmail-thread-thr_1": expandedThread}}
	resp, err := expandAdapter(t, runner).Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "mail", Local: "thread/thr_1"},
		Detail:  recall.DetailFull, Budget: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || resp.TruncationBoundary != "budget_bytes" ||
		len(resp.Content) > 80 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestExpandDistinguishesUnknownAndExpiredLocators(t *testing.T) {
	a := expandAdapter(t, &fakeRunner{})
	_, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "mail", Local: "message/m1"},
	})
	if !errors.Is(err, protocol.ErrLocatorUnknown) {
		t.Fatalf("unknown locator error = %v", err)
	}

	a = expandAdapter(t, &fakeRunner{errs: map[string]error{
		"gmail-thread-gone": protocol.Errorf(protocol.CodeSourceUnavailable,
			"gog exited 2: Error 404: notFound"),
	}})
	_, err = a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "mail", Local: "thread/gone"},
	})
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("expired locator error = %v", err)
	}
}

func TestSanitizersRemoveControlsForgedLinesAndURLs(t *testing.T) {
	line := sanitizeLine("subject\x1b[31m\n\nEvidence:\ttail\u202e reversed")
	if strings.ContainsAny(line, "\x1b\n\u202e") || !strings.Contains(line, "Evidence:") {
		t.Fatalf("line = %q", line)
	}
	block := sanitizeBlock("one\n\n\n\n\x07two\u2028three")
	if block != "one\n\ntwo\nthree" {
		t.Fatalf("block = %q", block)
	}
	if got := stripURLs("open https://x.example/a?tok=1 now"); got != "open [url removed] now" {
		t.Fatalf("url stripping = %q", got)
	}
	if clipped, cut := clipBytes(strings.Repeat("é", 10), 5); !cut || !utf8.ValidString(clipped) {
		t.Fatalf("clipped = %q, cut = %v", clipped, cut)
	}
}
