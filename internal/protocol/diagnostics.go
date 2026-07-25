package protocol

import (
	"bufio"
	"io"
	"sync"
)

// Diagnostics captures an adapter's stderr and its protocol violations.
//
// stderr is free-form adapter logging. It is stored and never parsed: no line
// written there can answer a request, complete a handshake, or change an
// outcome. That separation is the whole reason the protocol lives on stdout
// alone, so it is enforced by there being no code path from here to a decoder.
//
// The buffer is bounded in both directions. A chatty adapter cannot grow
// Recall's memory, and a single enormous line cannot either; what was dropped
// is counted so the report stays honest.
type Diagnostics struct {
	mu         sync.Mutex
	lines      []string
	maxLines   int
	maxLine    int
	dropped    int
	violations int
	lastError  string
}

// Diagnostics defaults. A few dozen recent lines is what an operator reads;
// more than that belongs in the adapter's own log.
const (
	DefaultDiagnosticLines  = 64
	DefaultDiagnosticLenCap = 4096
)

// DiagStoreIdentity is the health diagnostic under which an adapter names the
// store one instance actually opened.
//
// It exists because "two sources, one store" is a configuration error that no
// single source can see. Lineage groups on source_uid plus source_record_id,
// so one record read twice through two instances arrives as two independent
// pieces of evidence and collects the corroboration bonus for agreeing with
// itself. That has now happened three times in this system — overlapping
// document roots, two catalog instances over one server, and two td sources
// resolving to one database — reached by three different routes, so the check
// belongs somewhere that sees every source at once. `recall doctor` reads this
// key and refuses a profile in which two enabled instances of one adapter
// report the same value.
//
// Setting it is a CLAIM OF EXCLUSIVITY: this store is mine alone. An adapter
// for which two instances over one store is a legitimate configuration — one
// serving different views of a shared catalog, whose candidates collapse on a
// content fingerprint — must leave it unset, because for that adapter the
// overlap is the design rather than the defect. Absent means "makes no such
// claim", never "unknown".
//
// The value is opaque and compared only for equality between instances of the
// SAME adapter, so an adapter may use whatever names its store precisely: a
// resolved filesystem path, a database identity, an endpoint plus a namespace.
// It must name the store the adapter OPENED, never the one configuration asked
// for — a value copied from configuration would compare equal exactly when the
// configuration is consistent, which is the case that was never in doubt.
const DiagStoreIdentity = "store_identity"

// NewDiagnostics returns a bounded capture buffer.
func NewDiagnostics() *Diagnostics {
	return &Diagnostics{maxLines: DefaultDiagnosticLines, maxLine: DefaultDiagnosticLenCap}
}

// Capture reads r to EOF, storing each line. It blocks, so callers run it on
// its own goroutine for the life of the process.
func (d *Diagnostics) Capture(r io.Reader) {
	sc := bufio.NewScanner(r)
	// A single oversized stderr line must not stop the capture, so the scanner
	// gets room to split long lines rather than failing on them.
	sc.Buffer(make([]byte, 0, 8192), d.maxLine)
	for sc.Scan() {
		d.Record(sc.Text())
	}
	if err := sc.Err(); err != nil {
		d.Record("[recall] stderr capture ended: " + err.Error())
	}
}

// Record stores one line, truncating it and evicting the oldest as needed.
func (d *Diagnostics) Record(line string) {
	if len(line) > d.maxLine {
		line = line[:d.maxLine] + "…[truncated]"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.lines) == d.maxLines {
		d.lines = append(d.lines[:0], d.lines[1:]...)
		d.dropped++
	}
	d.lines = append(d.lines, line)
}

// RecordViolation notes a frame that could not be used. Violations are counted
// separately from stderr because they mean the adapter's stdout is not clean,
// which is a contract break rather than logging.
func (d *Diagnostics) RecordViolation(err error) {
	d.mu.Lock()
	d.violations++
	d.lastError = err.Error()
	d.mu.Unlock()
}

// Lines returns a copy of the captured stderr.
func (d *Diagnostics) Lines() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.lines))
	copy(out, d.lines)
	return out
}

// Violations returns how many frames were unusable.
func (d *Diagnostics) Violations() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.violations
}

// Dropped returns how many stderr lines were evicted.
func (d *Diagnostics) Dropped() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// Map renders the capture for a diagnostics field on a result or a health
// report. It is a snapshot: callers may keep it without holding a lock.
func (d *Diagnostics) Map() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.lines) == 0 && d.violations == 0 && d.dropped == 0 {
		return nil
	}
	out := make(map[string]any, 4)
	if len(d.lines) > 0 {
		lines := make([]string, len(d.lines))
		copy(lines, d.lines)
		out["stderr"] = lines
	}
	if d.dropped > 0 {
		out["stderr_dropped"] = d.dropped
	}
	if d.violations > 0 {
		out["protocol_violations"] = d.violations
		out["last_protocol_error"] = d.lastError
	}
	return out
}
