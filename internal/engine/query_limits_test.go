package engine

import (
	"strings"
	"testing"
	"time"
)

// A bounded repetition of a nullable expression used to iterate once per
// count for every dictionary term; the matcher now stops at the fixed point.
func TestLuceneRegexpHugeRepeatIsFast(t *testing.T) {
	for _, pattern := range []string{"(a?){1,2000000000}", "(){2000000000}", "(a*){1000000000}", "(ab|a?){999999999,}"} {
		re, msg := compileLuceneRegexp(pattern, reFlagAll, false)
		if re == nil {
			t.Fatalf("%s: %s", pattern, msg)
		}
		start := time.Now()
		if want := pattern != "(){2000000000}"; re.Matches("aaaa") != want {
			t.Fatalf("%s matches aaaa: %v", pattern, !want)
		}
		if !re.Matches("") {
			t.Fatalf("%s should match the empty string", pattern)
		}
		if re.Matches("aaab") && pattern != "(ab|a?){999999999,}" {
			t.Fatalf("%s should not match aaab", pattern)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s took %v", pattern, d)
		}
	}
	// exact counts still hold
	re, _ := compileLuceneRegexp("(a?){3}", reFlagAll, false)
	if !re.Matches("aa") || re.Matches("aaaa") {
		t.Fatal("(a?){3} bounds")
	}
	re, _ = compileLuceneRegexp("a{2,3}", reFlagAll, false)
	if re.Matches("a") || !re.Matches("aaa") || re.Matches("aaaa") {
		t.Fatal("a{2,3} bounds")
	}
	if re, msg := compileLuceneRegexp("a{99999999999999999999}", reFlagAll, false); re != nil || !strings.Contains(msg, "too large") {
		t.Fatalf("overflowing repetition: %v %q", re, msg)
	}
}

// Long flat patterns and deep nesting must not recurse without bound.
func TestLuceneRegexpLongPatterns(t *testing.T) {
	long := strings.Repeat("a", 200000)
	re, msg := compileLuceneRegexp(long, reFlagAll, false)
	if re == nil {
		t.Fatal(msg)
	}
	if !re.Matches(long) || re.Matches(long[1:]) {
		t.Fatal("long literal concat")
	}
	alts := strings.Repeat("a|", 100000) + "b"
	re, _ = compileLuceneRegexp(alts, reFlagAll, false)
	if !re.Matches("b") || re.Matches("c") {
		t.Fatal("long union")
	}
	deep := strings.Repeat("(", 5000) + "a" + strings.Repeat(")", 5000)
	if re, msg := compileLuceneRegexp(deep, reFlagAll, false); re != nil || !strings.Contains(msg, "nested") {
		t.Fatalf("deep nesting: %v %q", re, msg)
	}
	ok := strings.Repeat("(", 500) + "a" + strings.Repeat(")", 500)
	if re, msg := compileLuceneRegexp(ok, reFlagAll, false); re == nil || !re.Matches("a") {
		t.Fatalf("500 groups: %q", msg)
	}
}

// Lexing a query_string is linear in its length.
func TestQueryStringLexerIsLinear(t *testing.T) {
	q := strings.Repeat("term ", 200000)
	start := time.Now()
	toks, serr := lexQueryString(q)
	if serr != nil {
		t.Fatal(serr)
	}
	if len(toks) < 200000 {
		t.Fatalf("%d tokens", len(toks))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("lexing took %v", d)
	}
}
