package engine

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A pattern with many stars must not backtrack exponentially.
func TestWildcardMatchIsLinear(t *testing.T) {
	pattern := strings.Repeat("*a", 12) + "*b"
	s := strings.Repeat("a", 40)
	start := time.Now()
	if wildcardMatch(pattern, s) {
		t.Fatalf("%q should not match %q", pattern, s)
	}
	if !wildcardMatch(pattern, s+"b") {
		t.Fatalf("%q should match %q", pattern, s+"b")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("matching took %v", d)
	}
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "anything", true}, {"a*", "abc", true}, {"a*", "b", false},
		{"*c", "abc", true}, {"*c", "abd", false}, {"a*c", "ac", true}, {"a*c", "abbbc", true}, {"a*c", "abbbd", false},
		{"?", "a", true}, {"?", "", false}, {"a?c", "abc", true}, {"a?c", "ac", false}, {"a?c", "abbc", false},
		{"*?", "", false}, {"*?", "x", true}, {"?*", "x", true}, {"**", "", true}, {"a**b", "ab", true}, {"a**b", "a", false},
		{"abc", "abc", true}, {"abc", "abd", false}, {"", "", true}, {"", "a", false}, {"*a*", "banana", true}, {"*x*", "banana", false},
		{"t*.keyword", "title.keyword", true}, {"t*.keyword", "title.raw", false},
	} {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// An absurd compression must neither allocate gigabytes nor panic.
func TestMergingDigestHugeCompression(t *testing.T) {
	for _, c := range []float64{1e7, 1e300} {
		td := newMergingDigest(c)
		for i := 0; i < 100; i++ {
			td.add(float64(i), 1)
		}
		if q := td.quantile(0.5); q < 49 || q > 50 {
			t.Fatalf("compression %g: median %v", c, q)
		}
	}
}

// Invalid formats are not cached and the cache stays bounded.
func TestDateFormatCacheBounded(t *testing.T) {
	if f := ParseDateFormat("yyyy-MM-dd'"); f.err == nil {
		t.Fatalf("expected an invalid format")
	}
	dateFormatCacheMu.RLock()
	_, cached := dateFormatCache["yyyy-MM-dd'"]
	dateFormatCacheMu.RUnlock()
	if cached {
		t.Fatalf("an invalid format was cached")
	}
	for i := 0; i < 2*maxDateFormatCache; i++ {
		ParseDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS'" + strings.Repeat("x", i%50) + strconv.Itoa(i) + "'")
	}
	dateFormatCacheMu.RLock()
	n := len(dateFormatCache)
	dateFormatCacheMu.RUnlock()
	if n > maxDateFormatCache {
		t.Fatalf("cache holds %d entries", n)
	}
}

// User integers are converted through the 32-bit range whatever the
// platform.
func TestJavaIntClamp(t *testing.T) {
	for v, want := range map[float64]int{1e10: math.MaxInt32, -1e10: math.MinInt32, math.NaN(): 0, 3.9: 3, -3.9: -3, math.Inf(1): math.MaxInt32} {
		if got := javaInt(v); got != want {
			t.Errorf("javaInt(%v) = %d, want %d", v, got, want)
		}
	}
	if n, err := (objFields{}).intValue(M{"n": 1e10}, "n"); err != nil || n != math.MaxInt32 {
		t.Fatalf("intValue(1e10) = %d, %v", n, err)
	}
	if n, err := javaIntValue(-1e10); err != nil || n != math.MinInt32 {
		t.Fatalf("javaIntValue(-1e10) = %d, %v", n, err)
	}
	if n := getInt(M{"n": 1e10}, "n", 0); n != math.MaxInt32 {
		t.Fatalf("getInt(1e10) = %d", n)
	}
}

// The HALF_EVEN tie decision, now computed lazily, is unchanged.
func TestDecimalFormatTies(t *testing.T) {
	for _, c := range []struct {
		pattern string
		v       float64
		want    string
	}{
		{"0.0", 0.15, "0.1"}, {"0.0", 0.25, "0.2"}, {"0.0", 0.35, "0.3"}, {"0.0", 0.45, "0.5"},
		{"0", 2.5, "2"}, {"0", 3.5, "4"}, {"0.00", 0.125, "0.12"}, {"0.00", 0.375, "0.38"},
		{"#,##0.00", 1234567.891, "1,234,567.89"}, {"0.0", 0, "0.0"}, {"0", -2.5, "-2"},
	} {
		vf, err := decimalValueFormat(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if got := vf.decimalString(c.v); got != c.want {
			t.Errorf("format %q of %v = %q, want %q", c.pattern, c.v, got, c.want)
		}
	}
	// a pattern without digit placeholders is a prefix
	vf, _ := decimalValueFormat("abc")
	if got := vf.decimalString(3.7); got != "abc4" {
		t.Errorf("prefix pattern: %q", got)
	}
}

// Zone lookups are cached, misses included, and the cache stays bounded.
func TestLoadLocationCache(t *testing.T) {
	a, err := loadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := loadLocation("Asia/Tokyo"); b != a {
		t.Fatal("second lookup was not served from the cache")
	}
	if _, err := loadLocation("No/Such_Zone"); err == nil {
		t.Fatal("unknown zone accepted")
	}
	if _, ok := locationCache.Load("No/Such_Zone"); !ok {
		t.Fatal("miss not cached")
	}
	for i := 0; i < 2*maxLocationCache; i++ {
		loadLocation("Bogus/Zone" + strconv.Itoa(i))
	}
	n := 0
	locationCache.Range(func(_, _ any) bool { n++; return true })
	if n > maxLocationCache {
		t.Fatalf("cache holds %d entries", n)
	}
}

// The time value literals -0*1 and 0+ are matched without regexps.
func TestTimeValueLiterals(t *testing.T) {
	for s, want := range map[string]bool{"-1": true, "-01": true, "-0001": true, "-0": false, "1": false, "-": false, "-10": false, "": false} {
		if got := isMinusOneLiteral(s); got != want {
			t.Errorf("isMinusOneLiteral(%q) = %v", s, got)
		}
	}
	for s, want := range map[string]bool{"0": true, "000": true, "": false, "01": false, "-0": false} {
		if got := isZeroLiteral(s); got != want {
			t.Errorf("isZeroLiteral(%q) = %v", s, got)
		}
	}
}

// The median absolute deviation adds each centroid's deviation with its
// weight (InternalMedianAbsoluteDeviation) and approximates the exact value.
func TestMedianAbsoluteDeviationWeighted(t *testing.T) {
	td := newMergingDigest(100)
	var vals []float64
	for i := 0; i < 1000; i++ {
		v := float64(i%37) * 1.5
		vals = append(vals, v)
		td.add(v, 1)
	}
	sort.Float64s(vals)
	median := vals[len(vals)/2]
	devs := make([]float64, len(vals))
	for i, v := range vals {
		devs[i] = math.Abs(v - median)
	}
	sort.Float64s(devs)
	exact := devs[len(devs)/2]
	if got := medianAbsoluteDeviation(td, 100); math.IsNaN(got) || math.Abs(got-exact) > exact*0.1 {
		t.Fatalf("mad = %v, exact %v", got, exact)
	}
}
