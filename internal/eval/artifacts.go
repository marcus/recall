package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Artifact file names. A run is read by people and by tooling, so it writes
// both: run.json for comparison, cases.jsonl for the per-case detail that says
// why a number moved, summary.md for a person.
const (
	FileRun     = "run.json"
	FileCases   = "cases.jsonl"
	FileSummary = "summary.md"
)

// WriteRun writes a run's artifacts into dir.
//
// Artifacts carry excerpts and locators even when the pack carries only
// references, so they inherit the pack's sensitivity. They belong in the state
// directory and never in the repository — see docs/evaluation.md#layout.
func WriteRun(dir string, run Run, results []CaseResult, scores []CaseScore) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	SortResults(results)
	sort.Slice(scores, func(i, j int) bool { return scores[i].CaseID < scores[j].CaseID })

	if err := writeJSON(filepath.Join(dir, FileRun), run); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(dir, FileCases), results, scores); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FileSummary), []byte(Summarize(run, scores)), 0o600)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// caseRecord pairs what happened with what it scored, so one line of
// cases.jsonl answers both "what did it return" and "what did that earn".
type caseRecord struct {
	Result CaseResult `json:"result"`
	Score  CaseScore  `json:"score"`
}

func writeJSONL(path string, results []CaseResult, scores []CaseScore) error {
	byID := make(map[string]CaseScore, len(scores))
	for _, s := range scores {
		byID[s.CaseID] = s
	}

	var b strings.Builder
	for _, r := range results {
		line, err := json.Marshal(caseRecord{Result: r, Score: byID[r.CaseID]})
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// DescribeHost captures the parts of the environment that can move a number.
func DescribeHost() Environment {
	return Environment{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}
}

// Summarize renders a run for a person.
//
// Gates come first and metrics second, because a run whose gates failed is not
// evidence about its metrics — putting the scores at the top would invite
// reading them anyway.
func Summarize(run Run, scores []CaseScore) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s — %s\n\n", run.Pack.PackID, run.Status)
	fmt.Fprintf(&b, "- run: `%s`\n", run.RunID)
	fmt.Fprintf(&b, "- pack: `%s` version %s, content `%s`\n",
		run.Pack.PackID, run.Pack.Version, short(run.Pack.ContentHash))
	fmt.Fprintf(&b, "- commit: `%s`%s\n", short(run.Environment.RecallCommit), dirty(run.Environment))
	fmt.Fprintf(&b, "- cases: %d\n", len(scores))
	fmt.Fprintf(&b, "- elapsed: %s\n\n", run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond))

	b.WriteString("## Gates\n\n")
	if len(run.Gates) == 0 {
		b.WriteString("None evaluated.\n\n")
	}
	for _, g := range run.Gates {
		fmt.Fprintf(&b, "- **%s** — %s", g.Name, g.Status)
		if g.Observed != nil {
			fmt.Fprintf(&b, " (observed %.4g", *g.Observed)
			if g.Threshold != nil {
				fmt.Fprintf(&b, ", threshold %.4g", *g.Threshold)
			}
			b.WriteString(")")
		}
		if g.Detail != "" {
			fmt.Fprintf(&b, " — %s", g.Detail)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Metrics\n\n")
	writeRates(&b, "overall", run.Metrics.Overall.Rates)

	if len(run.Metrics.ByTag.Groups) > 0 {
		b.WriteString("\n### By tag\n\n")
		b.WriteString("A macro average is here so one large group cannot hide another's regression.\n\n")
		for _, name := range sortedKeys(run.Metrics.ByTag.Groups) {
			writeRates(&b, name, run.Metrics.ByTag.Groups[name].Rates)
		}
		writeRates(&b, "macro", run.Metrics.ByTag.Macro.Rates)
	}
	return b.String()
}

func writeRates(b *strings.Builder, label string, r Rates) {
	fmt.Fprintf(b, "**%s** — ", label)
	parts := []string{
		meanText("nDCG@10", r.NDCG10),
		meanText("R@5", r.Recall5),
		meanText("MRR@10", r.MRR10),
		meanText("S@5", r.Success5),
		meanText("abstention", r.AbstentionAccuracy),
		meanText("locators", r.LocatorSuccess),
	}
	b.WriteString(strings.Join(parts, ", "))
	b.WriteString("\n")
}

// meanText renders a mean, or says it is undefined. An undefined metric is
// never printed as 0: that is the difference between "nothing to find" and
// "found nothing".
func meanText(label string, m Mean) string {
	if !m.Defined() {
		return label + " n/a"
	}
	return fmt.Sprintf("%s %.4f (n=%d)", label, m.Value, m.N)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func dirty(e Environment) string {
	if e.Dirty {
		return " (dirty tree)"
	}
	return ""
}
