package claracorpus

import (
	"fmt"
	"math"
	"time"
)

// decayFloor bounds how far a faded record may be demoted in local ranking.
//
// docs/spec.md#decay requires faded material to stay recoverable and
// searchable, so the multiplier spans [decayFloor, 1] rather than [0, 1]: at
// worst a fully faded record ranks as though its text had matched half as well,
// never as though it had not matched at all. A multiplier reaching zero would
// turn "this memory no longer carries weight" into "this memory does not
// exist", and the corpus does not say that — Clara's own consolidate archives a
// faded record rather than deleting it.
const decayFloor = 0.5

// decay is Clara's effective-weight calculation together with every input it
// used.
//
// The inputs travel with the answer because the answer is a number nobody can
// check on sight. A candidate carrying effective_weight 0.34 says nothing; one
// carrying weight 0.7, half_life_days 45, age_days 60, aged from last_seen, and
// the formula says what happened and lets a reader disagree with it.
type decay struct {
	// Weight is the record's base importance, 0..1.
	Weight float64
	// HalfLife is the record's own half_life_days. Nil means the record does
	// not decay — Clara's read-time rule, and deliberately NOT a cue to look up
	// Record::DEFAULT_HALF_LIFE, which is a write-time default consulted when a
	// record is created and never again. Applying it here would be a second
	// decay model contradicting the first, on exactly the records Clara decided
	// should not decay.
	HalfLife *float64
	// AgeDays is whole days from Basis to the corpus's civil today, floored at
	// zero: a record stamped in the future is not negatively aged.
	AgeDays int
	// Basis names the field the age was measured from — last_seen when it is
	// present, created otherwise, which is Clara's own order.
	Basis string
	// Effective is weight × 0.5^(age/half_life), or weight when HalfLife is nil.
	Effective float64
}

// effectiveDecay computes Clara's Memory::Decay.effective for one record.
//
//	effective = weight × 0.5 ** (age_in_days / half_life_days)
//
// This is the corpus's authority, reproduced and not reinterpreted. Clara ages
// from `last_seen` and falls back to `created`; a nil or non-positive half-life
// returns the weight unchanged.
func effectiveDecay(weight float64, halfLife *float64, lastSeen, created civilDate, today civilDate) decay {
	d := decay{Weight: weight, HalfLife: halfLife, Effective: weight, Basis: "last_seen"}
	basis := lastSeen
	if basis.zero() {
		basis, d.Basis = created, "created"
	}
	if basis.zero() {
		d.Basis = "none"
	}
	d.AgeDays = basis.daysUntil(today)
	if halfLife == nil || *halfLife <= 0 {
		return d
	}
	d.Effective = weight * math.Pow(0.5, float64(d.AgeDays)/(*halfLife))
	return d
}

// multiplier is what decay does to local rank. See [decayFloor].
func (d decay) multiplier() float64 {
	e := d.Effective
	if e < 0 {
		e = 0
	}
	if e > 1 {
		e = 1
	}
	return decayFloor + (1-decayFloor)*e
}

// explain renders the calculation as one line of evidence.
func (d decay) explain() string {
	if d.HalfLife == nil {
		return "no decay: half_life_days is null, so this record keeps its weight"
	}
	return fmt.Sprintf("weight %.3g × 0.5^(%d days / %.6g) = %.4g",
		d.Weight, d.AgeDays, *d.HalfLife, d.Effective)
}

// describe writes the calculation into candidate metadata.
func (d decay) describe(into map[string]any) {
	into["weight"] = round(d.Weight, 4)
	into["effective_weight"] = round(d.Effective, 4)
	into["age_days"] = d.AgeDays
	into["decay_basis"] = d.Basis
	into["decay"] = d.explain()
	if d.HalfLife != nil {
		into["half_life_days"] = round(*d.HalfLife, 6)
	}
}

// round trims a computed float for display. The stored value is exact; this
// only keeps a metadata field from carrying eighteen digits of an arithmetic
// artifact into a terminal.
func round(v float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
}

// civilDate is a Clara YYYY-MM-DD date. Clara's stores use civil dates, not
// instants, for everything reinforcement and decay touch, and the distinction
// matters: an age in whole days is not the same quantity as a duration.
type civilDate struct {
	y  int
	m  time.Month
	d  int
	ok bool
}

func parseCivil(s string) civilDate {
	if len(s) < 10 {
		return civilDate{}
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return civilDate{}
	}
	return civilDate{y: t.Year(), m: t.Month(), d: t.Day(), ok: true}
}

func civilOf(t time.Time) civilDate {
	return civilDate{y: t.Year(), m: t.Month(), d: t.Day(), ok: true}
}

func (c civilDate) zero() bool { return !c.ok }

func (c civilDate) String() string {
	if !c.ok {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", c.y, int(c.m), c.d)
}

// at renders the date as the instant it began in the corpus's zone. A civil
// date is not an instant, so every conversion has to name the zone it used;
// this is the one place that happens.
func (c civilDate) at(loc *time.Location) time.Time {
	return time.Date(c.y, c.m, c.d, 0, 0, 0, 0, loc)
}

// daysUntil counts whole days from c to later, floored at zero. Clara's
// Decay.age_days does the same, including the floor: a record stamped in the
// future is aged zero rather than reinforced by arithmetic.
func (c civilDate) daysUntil(later civilDate) int {
	if !c.ok || !later.ok {
		return 0
	}
	from := time.Date(c.y, c.m, c.d, 0, 0, 0, 0, time.UTC)
	to := time.Date(later.y, later.m, later.d, 0, 0, 0, 0, time.UTC)
	days := int(to.Sub(from).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// after reports whether c is strictly later than other. A zero date is never
// later than anything, so it never wins a "newest" comparison.
func (c civilDate) after(other civilDate) bool {
	switch {
	case !c.ok:
		return false
	case !other.ok:
		return true
	case c.y != other.y:
		return c.y > other.y
	case c.m != other.m:
		return c.m > other.m
	default:
		return c.d > other.d
	}
}
