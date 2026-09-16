package engine

import (
	"testing"
	"time"
)

// TestValidOffsetsDSTBoundaries checks validOffsets at the two kinds of DST
// edge in America/New_York: a local time skipped by the spring-forward gap
// (no valid offset) and a local time repeated by the fall-back overlap (two
// valid offsets, most recent first), plus ordinary summer and winter times
// with exactly one.
func TestValidOffsetsDSTBoundaries(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York not available: %v", err)
	}
	local := func(y int, m time.Month, d, h, mi int) int64 {
		return time.Date(y, m, d, h, mi, 0, 0, time.UTC).UnixMilli()
	}
	if got := validOffsets(local(2021, 3, 14, 2, 30), ny); len(got) != 0 {
		t.Errorf("spring-forward gap 2021-03-14 02:30: validOffsets = %v, want none", got)
	}
	if got := validOffsets(local(2020, 11, 1, 1, 30), ny); len(got) != 2 || got[0] != -4*3600 || got[1] != -5*3600 {
		t.Errorf("fall-back overlap 2020-11-01 01:30: validOffsets = %v, want [-14400 -18000]", got)
	}
	if got := validOffsets(local(2021, 6, 1, 12, 0), ny); len(got) != 1 || got[0] != -4*3600 {
		t.Errorf("ordinary EDT: validOffsets = %v, want [-14400]", got)
	}
	if got := validOffsets(local(2021, 1, 1, 12, 0), ny); len(got) != 1 || got[0] != -5*3600 {
		t.Errorf("ordinary EST: validOffsets = %v, want [-18000]", got)
	}
}

// TestDateRoundingHourAcrossFallBackDST rounds to the hour on both sides of
// the America/New_York fall-back transition, the exact scenario of a
// windows-latest CI hang inside periodAt/validOffsets (TestAggregationFidelityDatesRangesPipelines
// exercises the same case through a full date_histogram request; this pins
// it at the dateRounding level).
func TestDateRoundingHourAcrossFallBackDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York not available: %v", err)
	}
	r := &dateRounding{unit: unitHour, loc: ny}
	cases := []struct {
		name    string
		instant time.Time
		want    time.Time
	}{
		{"first 01:xx EDT", time.Date(2020, 11, 1, 5, 45, 0, 0, time.UTC), time.Date(2020, 11, 1, 5, 0, 0, 0, time.UTC)},
		{"second 01:xx EST", time.Date(2020, 11, 1, 6, 45, 0, 0, time.UTC), time.Date(2020, 11, 1, 6, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := r.round(tc.instant.UnixMilli()); got != tc.want.UnixMilli() {
			t.Errorf("%s: round(%s) = %d, want %d", tc.name, tc.instant, got, tc.want.UnixMilli())
		}
	}
}

// TestValidOffsetsManyYearsCompletes sweeps periodAt/validOffsets across
// decades for several zones (an ordinary DST zone, a DST-free zone, a
// non-hour offset, and UTC). periodAt's neighbour-merging loops and
// validOffsets' own loop are bounded by maxPeriodMerge and require forward
// progress on every step specifically so this kind of sweep cannot hang if a
// platform's time.Time.ZoneBounds() ever fails to behave as documented (see
// periodAt) — which is what made windows-latest CI hang for 600s inside this
// exact call chain before those guards existed. This only proves the sweep
// completes; it does not reproduce the platform-specific trigger, which did
// not reproduce outside windows-latest CI.
func TestValidOffsetsManyYearsCompletes(t *testing.T) {
	for _, name := range []string{"America/New_York", "Asia/Tokyo", "Pacific/Chatham", "UTC"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("%s not available: %v", name, err)
		}
		for year := 1970; year < 2035; year++ {
			for month := 1; month <= 12; month++ {
				local := time.Date(year, time.Month(month), 15, 12, 0, 0, 0, time.UTC).UnixMilli()
				if offs := validOffsets(local, loc); len(offs) == 0 {
					t.Errorf("%s %04d-%02d-15 12:00: no valid offset", name, year, month)
				}
			}
		}
	}
}
