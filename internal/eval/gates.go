package eval

import (
	"fmt"
	"time"
)

// Gate names. They are stable identifiers because a run artifact records them
// and a comparison matches on them.
const (
	GateSensitivity     = "no_sensitivity_leaks"
	GateForbidden       = "no_forbidden_evidence"
	GateLocators        = "locators_resolve"
	GateSourceOutcomes  = "source_outcomes_honest"
	GateCoverage        = "coverage_honest"
	GateExactIdentifier = "exact_identifier_success"
	GateAbstention      = "abstention_accuracy"
	GateLatency         = "p95_latency"
	GateAssertions      = "declared_assertions"
	GateRunIntegrity    = "run_integrity"
)

// ExactTag marks the cases whose Success@1 the exact-identifier gate measures.
const ExactTag = "exact"

// EvaluateGates decides whether a run is valid at all.
//
// A gate is not a metric. Metrics order experiments; gates decide whether a run
// is admissible evidence, and a failed gate invalidates the run however good
// its scores are. That asymmetry is the whole point: a change that improves
// ranking while leaking a restricted record has not improved anything.
//
// A threshold the pack does not state is not silently invented. Those gates
// report skipped, because a bar nobody set is not a bar something can fail —
// and inventing one would let the first run define its own passing grade.
func EvaluateGates(pack *Pack, scores []CaseScore, report Report, integrity []string) []Gate {
	var gates []Gate

	gates = append(gates, countGate(GateSensitivity, sensitivityLeaks(scores),
		"returned candidates above the ceiling a case asserted"))
	gates = append(gates, forbiddenGate(scores))
	gates = append(gates, rateGate(GateLocators, report.Overall.LocatorSuccess, 1.0,
		"every returned reference must expand to the judged lineage and revision"))
	gates = append(gates, rateGate(GateSourceOutcomes, report.Overall.SourceOutcomeAccuracy, 1.0,
		"failure, denial, timeout, and partial coverage must be reported as they happened"))
	gates = append(gates, rateGate(GateCoverage, report.Overall.CoverageAccuracy, 1.0,
		"degraded coverage must be reported when and only when it is true"))
	exactThreshold, exactDeclared := pack.Thresholds["exact_identifier_success_at_1"]
	gates = append(gates, thresholdGate(GateExactIdentifier, exactSuccess(scores),
		exactThreshold, exactDeclared,
		"an identifier query must put its record first"))
	abstentionThreshold, abstentionDeclared := pack.Thresholds["abstention_accuracy"]
	gates = append(gates, thresholdGate(GateAbstention, report.Overall.AbstentionAccuracy,
		abstentionThreshold, abstentionDeclared,
		"answering when it should abstain, or abstaining when it should answer"))
	gates = append(gates, latencyGate(pack, report))
	gates = append(gates, countGate(GateAssertions, countAssertionViolations(scores),
		"declared case assertion violation(s)"))
	gates = append(gates, integrityGate(integrity))

	return gates
}

func countAssertionViolations(scores []CaseScore) int {
	n := 0
	for _, score := range scores {
		n += len(score.AssertionViolations)
	}
	return n
}

// sensitivityLeaks counts every candidate that crossed a ceiling a case stated.
func sensitivityLeaks(scores []CaseScore) int {
	n := 0
	for _, s := range scores {
		n += s.SensitivityViolations
	}
	return n
}

// forbiddenGate fails on any forbidden evidence in the top 5.
//
// Forbidden@5 is a rate elsewhere, but here it is a count: one superseded
// record presented as current is a defect, and averaging it across a pack would
// let a large pack dilute it below notice.
func forbiddenGate(scores []CaseScore) Gate {
	violations := 0
	for _, s := range scores {
		if s.Forbidden5.OK && s.Forbidden5.V > 0 {
			violations++
		}
	}
	return countGate(GateForbidden, violations, "cases surfacing forbidden or superseded evidence")
}

func exactSuccess(scores []CaseScore) Mean {
	var m Mean
	for _, s := range scores {
		if !hasTag(s.Tags, ExactTag) || !s.MRR10.OK {
			continue
		}
		m.N++
		// Success@1 is MRR with the record in first place, which is the only
		// position an exact identifier lookup has any business landing in.
		if s.MRR10.V == 1 {
			m.Value += 1
		}
	}
	if m.N > 0 {
		m.Value /= float64(m.N)
	}
	return m
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func countGate(name string, count int, detail string) Gate {
	g := Gate{Name: name, Status: GatePass}
	observed := float64(count)
	zero := 0.0
	g.Observed, g.Threshold = &observed, &zero
	if count > 0 {
		g.Status = GateFail
		g.Detail = fmt.Sprintf("%d %s", count, detail)
	}
	return g
}

// rateGate holds a rate to a fixed bar. These are the gates whose bar is not a
// tuning decision: a locator either resolves or it does not.
func rateGate(name string, got Mean, want float64, detail string) Gate {
	g := Gate{Name: name, Threshold: &want}
	if !got.Defined() {
		g.Status = GateSkipped
		g.Detail = "no case measured it"
		return g
	}
	observed := got.Value
	g.Observed = &observed
	g.Status = GatePass
	if observed < want {
		g.Status = GateFail
		g.Detail = detail
	}
	return g
}

// thresholdGate holds a rate to a bar the pack states.
func thresholdGate(name string, got Mean, want float64, declared bool, detail string) Gate {
	g := Gate{Name: name}
	if !got.Defined() {
		g.Status = GateSkipped
		g.Detail = "no case measured it"
		return g
	}
	observed := got.Value
	g.Observed = &observed
	if !declared {
		// The pack states no bar. On a first run there is no baseline to
		// regress against either, so this records the number that will become
		// the bar rather than passing a test nobody wrote.
		g.Status = GateSkipped
		g.Detail = "pack states no threshold; this run is the evidence for one"
		return g
	}
	g.Threshold = &want
	g.Status = GatePass
	if observed < want {
		g.Status = GateFail
		g.Detail = detail
	}
	return g
}

func latencyGate(pack *Pack, report Report) Gate {
	g := Gate{Name: GateLatency}
	if pack.Budgets == nil || pack.Budgets.P95LatencyMS == 0 {
		g.Status = GateSkipped
		g.Detail = "pack states no latency budget"
		return g
	}
	want := float64(pack.Budgets.P95LatencyMS)
	g.Threshold = &want

	p95 := report.Overall.Latency.Warm.P95
	if report.Overall.Latency.Warm.N == 0 {
		p95 = report.Overall.Latency.Cold.P95
	}
	observed := float64(p95) / float64(time.Millisecond)
	g.Observed = &observed
	g.Status = GatePass
	if observed > want {
		g.Status = GateFail
		g.Detail = fmt.Sprintf("p95 %.1fms over the %.0fms budget", observed, want)
	}
	return g
}

// integrityGate fails on anything that makes the run itself untrustworthy:
// a mutated fixture, a crash, an undeclared network call, or a subprocess a
// project configuration caused to spawn.
func integrityGate(violations []string) Gate {
	g := Gate{Name: GateRunIntegrity, Status: GatePass}
	if len(violations) > 0 {
		g.Status = GateFail
		g.Detail = fmt.Sprintf("%d integrity violation(s): %v", len(violations), violations)
	}
	return g
}

// Valid reports whether every gate that ran passed.
//
// A skipped gate does not invalidate a run: it means the pack asked nothing of
// it. A failed gate does, whatever the metrics say.
func Valid(gates []Gate) bool {
	for _, g := range gates {
		if g.Status == GateFail {
			return false
		}
	}
	return true
}

// StatusOf reduces gates to a run status.
//
// A failed gate is invalid, not merely failed: the run is not evidence about
// anything, so it must not be compared with a baseline or used to justify a
// change.
func StatusOf(gates []Gate) Status {
	if Valid(gates) {
		return StatusPass
	}
	return StatusInvalid
}
