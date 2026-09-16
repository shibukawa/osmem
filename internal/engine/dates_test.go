package engine

import (
	"testing"
	"time"
)

func TestParseDateFormat(t *testing.T) {
	df := ParseDateFormat(DefaultDateFormat)
	for _, s := range []string{"2024", "2024-03", "2024-03-05", "2024-03-05T10:15", "2024-03-05T10:15:30", "2024-03-05T10:15:30.123Z", "2024-03-05T10:15:30+09:00", "1709596800000"} {
		if _, err := df.ParseString(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := df.ParseString("05/03/2024"); err == nil {
		t.Error("expected failure for dd/MM/yyyy under default format")
	}
	if tm, err := ParseDateFormat("dd/MM/yyyy||epoch_second").ParseString("05/03/2024"); err != nil || tm.Month() != 3 {
		t.Errorf("custom format: %v %v", tm, err)
	}
	if got := ParseDateFormat("yyyy-MM-dd").Format(time.Date(2024, 3, 5, 1, 2, 3, 0, time.UTC)); got != "2024-03-05" {
		t.Errorf("format: %s", got)
	}
	if got := df.Format(time.Date(2024, 3, 5, 1, 2, 3, 0, time.UTC)); got != "2024-03-05T01:02:03.000Z" {
		t.Errorf("default format: %s", got)
	}
}

func TestDateMath(t *testing.T) {
	now := time.Date(2024, 3, 15, 12, 30, 45, 0, time.UTC)
	df := ParseDateFormat(DefaultDateFormat)
	jst := time.FixedZone("JST", 9*3600)
	cases := []struct {
		expr    string
		loc     *time.Location
		roundUp bool
		want    time.Time
	}{
		{"now", time.UTC, false, now},
		{"now-1d", time.UTC, false, now.AddDate(0, 0, -1)},
		{"now/d", time.UTC, false, time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)},
		{"now/d", time.UTC, true, time.Date(2024, 3, 15, 23, 59, 59, 999e6, time.UTC)},
		{"now-1M/M", time.UTC, false, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"now+2h-30m", time.UTC, false, now.Add(90 * time.Minute)},
		{"2024-01-01||+1M/d", time.UTC, false, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)},
		// OpenSearch round-up parsing fills the missing day with 01 and the
		// time with 23:59:59.999999999
		{"2024-01", time.UTC, true, time.Date(2024, 1, 1, 23, 59, 59, 999999999, time.UTC)},
		{"2024-01-10", jst, false, time.Date(2024, 1, 10, 0, 0, 0, 0, jst)},
		{"2024-01-10T00:00:00Z", jst, false, time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)},
		{"now/d", jst, false, time.Date(2024, 3, 15, 0, 0, 0, 0, jst)},
		{"now/w", time.UTC, false, time.Date(2024, 3, 11, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := ParseDateMath(tc.expr, df, now, tc.loc, tc.roundUp)
		if err != nil {
			t.Errorf("%s: %v", tc.expr, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s (roundUp=%v): got %v want %v", tc.expr, tc.roundUp, got, tc.want)
		}
	}
	if _, err := ParseDateMath("now-1x", df, now, time.UTC, false); err == nil {
		t.Error("expected error for bad unit")
	}
}

func TestMinimumShouldMatch(t *testing.T) {
	cases := []struct {
		spec    string
		clauses int
		want    int
	}{
		{"2", 5, 2}, {"-1", 5, 4}, {"75%", 4, 3}, {"-25%", 4, 3}, {"3<90%", 2, 2}, {"3<90%", 10, 9}, {"2<-25% 9<-3", 12, 9}, {"10", 3, 10},
	}
	for _, tc := range cases {
		if got := minimumShouldMatch(tc.spec, tc.clauses); got != tc.want {
			t.Errorf("%s/%d: got %d want %d", tc.spec, tc.clauses, got, tc.want)
		}
	}
}

func TestWildcardAndSourceFilter(t *testing.T) {
	if !wildcardMatch("a*c", "abbbc") || wildcardMatch("a*c", "abd") || !wildcardMatch("a?c", "abc") || !wildcardMatch("*", "") {
		t.Error("wildcardMatch")
	}
	src := M{"a": M{"b": 1, "c": 2}, "d": []any{M{"e": 1, "f": 2}}, "g": "x"}
	got := sourceFilter{includes: []string{"a.b", "d.e"}}.apply(src)
	if got["g"] != nil || got["a"].(M)["c"] != nil || got["a"].(M)["b"] != 1 || got["d"].([]any)[0].(M)["e"] != 1 || got["d"].([]any)[0].(M)["f"] != nil {
		t.Errorf("includes: %v", got)
	}
	got = sourceFilter{excludes: []string{"a.*"}}.apply(src)
	if len(got["a"].(M)) != 0 || got["g"] != "x" {
		t.Errorf("excludes: %v", got)
	}
}
