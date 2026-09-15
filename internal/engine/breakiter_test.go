package engine

import (
	"fmt"
	"testing"
	"unicode/utf16"
)

// Expected boundaries were produced with java.text.BreakIterator
// (Locale.ROOT) on JDK 25, the runtime of OpenSearch 3.8.
func TestSentenceAndWordBreaksMatchJava(t *testing.T) {
	cases := []struct {
		kind, text string
		want       []int
	}{
		{"S", "Dogs are lazy. Foxes are quick.", []int{0, 15, 31}},
		{"S", "end. (Next word", []int{0, 5, 15}},
		{"S", "end.\"next", []int{0, 5, 9}},
		{"S", "end. 9 word", []int{0, 11}},
		{"S", "a?\"B", []int{0, 3, 4}},
		{"S", "a.)B", []int{0, 4}},
		{"S", "Mr. Smith? Yes.", []int{0, 4, 11, 15}},
		{"S", "x. éB", []int{0, 5}},
		{"S", "a?   b", []int{0, 4, 6}},
		{"S", "e.g. the fox. Mr. Fox went to Washington.", []int{0, 14, 18, 41}},
		{"W", "U.S.A. fox", []int{0, 5, 6, 7, 10}},
		{"W", "don't a--b 3.14 $5abc 100%", []int{0, 5, 6, 7, 8, 9, 10, 11, 15, 16, 21, 22, 26}},
		{"W", "hello\r\n\nworld", []int{0, 5, 7, 8, 13}},
		{"W", "あいアイ漢字", []int{0, 2, 4, 6}},
	}
	for _, c := range cases {
		it := newWordBreakIterator()
		if c.kind == "S" {
			it = newSentenceBreakIterator()
		}
		it.setText(utf16.Encode([]rune(c.text)))
		got := []int{it.first()}
		for p := it.next(); p != breakDone; p = it.next() {
			got = append(got, p)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s %q: boundaries %v, want %v", c.kind, c.text, got, c.want)
		}
	}
}

// preceding and following restart from a safe position found with the
// backward rules; a danda after a period makes following skip a boundary
// that forward iteration reports, as in Java.
func TestBreakIteratorFollowingMatchesJavaQuirks(t *testing.T) {
	text := utf16.Encode([]rune("é,wordßThex?.।?!theA.x1א."))
	it := newSentenceBreakIterator()
	it.setText(text)
	for p := it.next(); p != breakDone; p = it.next() {
	}
	// the same call sequence as the Java reference: preceding(i), then following(i)
	for i := 0; i <= len(text); i++ {
		it.preceding(i)
	}
	var fol []int
	for i := 0; i <= len(text); i++ {
		fol = append(fol, it.following(i))
	}
	want := []int{13, 13, 13, 13, 13, 13, 13, 13, 13, 13, 13, 13, 13, 16, 16, 16, 25, 25, 25, 25, 25, 25, 25, 25, 25, -1}
	if fmt.Sprint(fol) != fmt.Sprint(want) {
		t.Errorf("following = %v, want %v", fol, want)
	}
}
