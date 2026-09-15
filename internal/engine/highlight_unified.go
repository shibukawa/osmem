package engine

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

// The unified highlighter (OpenSearch's default) re-analyzes the field
// values, joined with U+0000, and marks the tokens matching the query
// terms, multi-term queries and phrases. Passages come from a break
// iterator applied to each value separately: the sentence iterator bounded
// by fragment_size (sentences are merged while they fit and oversized ones
// are cut at word boundaries around the match), the word iterator for
// boundary_scanner word, or whole values for number_of_fragments 0 and
// non-text fields. Every passage is scored with Lucene's BM25-like
// PassageScorer; the best number_of_fragments passages are returned in
// document order, or by score with order score.

// boundedBreakIterator is OpenSearch's BoundedBreakIteratorScanner: sentence
// breaks bounded by maxLen, refined with word breaks. Only preceding(offset)
// followed by following(offset-1) is supported, with increasing offsets.
type boundedBreakIterator struct {
	main, inner                                                       breakIterator
	maxLen                                                            int
	lastPrecedingOffset, windowStart, windowEnd, innerStart, innerEnd int
}

func newBoundedBreakIterator(maxLen int) *boundedBreakIterator {
	b := &boundedBreakIterator{main: newSentenceBreakIterator(), inner: newWordBreakIterator(), maxLen: maxLen}
	b.reset()
	return b
}

func (b *boundedBreakIterator) reset() {
	b.lastPrecedingOffset, b.windowStart, b.windowEnd, b.innerStart, b.innerEnd = -1, -1, -1, -1, 0
}

func (b *boundedBreakIterator) setText(text []uint16) {
	b.reset()
	b.main.setText(text)
	b.inner.setText(text)
}

func (b *boundedBreakIterator) first() int {
	b.innerStart = b.main.first()
	b.innerEnd = b.innerStart
	return b.innerStart
}

func (b *boundedBreakIterator) last() int    { return b.main.last() }
func (b *boundedBreakIterator) current() int { return b.innerEnd }
func (b *boundedBreakIterator) next() int    { return b.main.next() }

func (b *boundedBreakIterator) preceding(offset int) int {
	if offset > b.windowStart && offset < b.windowEnd {
		b.innerStart = b.innerEnd
		b.innerEnd = b.windowEnd
	} else {
		b.windowStart = b.main.preceding(offset)
		b.innerStart = b.windowStart
		b.windowEnd = b.main.following(offset - 1)
		b.innerEnd = b.windowEnd
		// expand to the next breaks while the passage fits
		for b.innerEnd-b.innerStart < b.maxLen {
			newEnd := b.main.following(b.innerEnd)
			if newEnd == breakDone || newEnd-b.innerStart > b.maxLen {
				break
			}
			b.windowEnd = newEnd
			b.innerEnd = newEnd
		}
	}
	if b.innerEnd-b.innerStart > b.maxLen {
		// too long: word boundaries left of the match first, then to the right
		if offset-b.maxLen > b.innerStart {
			b.innerStart = max(b.innerStart, b.inner.preceding(offset-b.maxLen))
		}
		remaining := max(0, b.maxLen-(offset-b.innerStart))
		if offset+remaining < b.windowEnd {
			b.innerEnd = b.inner.following(offset + remaining)
		}
	}
	b.lastPrecedingOffset = offset - 1
	return b.innerStart
}

func (b *boundedBreakIterator) following(int) int { return b.innerEnd }

// separatorBreakIterator is OpenSearch's CustomSeparatorBreakIterator: it
// only breaks after the separator character.
type separatorBreakIterator struct {
	sep  uint16
	text []uint16
	cur  int
}

func (s *separatorBreakIterator) setText(text []uint16) {
	s.text = text
	s.cur = 0
}

func (s *separatorBreakIterator) first() int {
	s.cur = 0
	return 0
}

func (s *separatorBreakIterator) last() int {
	s.cur = len(s.text)
	return s.cur
}

func (s *separatorBreakIterator) current() int { return s.cur }

func (s *separatorBreakIterator) next() int {
	if s.cur == len(s.text) {
		return breakDone
	}
	return s.forward()
}

func (s *separatorBreakIterator) forward() int {
	for i := s.cur + 1; i < len(s.text); i++ {
		if s.text[i] == s.sep {
			s.cur = i + 1
			return s.cur
		}
	}
	s.cur = len(s.text)
	return s.cur
}

func (s *separatorBreakIterator) following(pos int) int {
	if pos == len(s.text) {
		s.cur = pos
		return breakDone
	}
	s.cur = pos
	return s.forward()
}

func (s *separatorBreakIterator) preceding(pos int) int {
	if pos == 0 {
		s.cur = 0
		return breakDone
	}
	for i := pos - 1; i >= 0; i-- {
		if s.text[i] == s.sep {
			s.cur = i + 1
			return s.cur
		}
	}
	s.cur = 0
	return 0
}

// splittingBreakIterator is Lucene's SplittingBreakIterator: the text is
// sliced at every separator and the base iterator runs on one slice at a
// time.
type splittingBreakIterator struct {
	base                         breakIterator
	sep                          uint16
	text                         []uint16
	sliceStart, sliceEnd, cursor int
}

func indexOfUnit(text []uint16, c uint16, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(text); i++ {
		if text[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexOfUnit(text []uint16, c uint16, from int) int {
	if from >= len(text) {
		from = len(text) - 1
	}
	for i := from; i >= 0; i-- {
		if text[i] == c {
			return i
		}
	}
	return -1
}

func (s *splittingBreakIterator) setText(text []uint16) {
	s.text = text
	s.first()
}

func (s *splittingBreakIterator) first() int {
	s.sliceStart = 0
	s.sliceEnd = indexOfUnit(s.text, s.sep, 0)
	if s.sliceEnd == -1 {
		s.sliceEnd = len(s.text)
	}
	if s.sliceStart == s.sliceEnd {
		s.cursor = s.sliceStart
		return s.cursor
	}
	s.base.setText(s.text[s.sliceStart:s.sliceEnd])
	s.cursor = s.sliceStart + s.base.current()
	return s.cursor
}

func (s *splittingBreakIterator) last() int {
	s.sliceEnd = len(s.text)
	s.sliceStart = lastIndexOfUnit(s.text, s.sep, len(s.text)-1) + 1
	if s.sliceEnd == s.sliceStart {
		s.cursor = s.sliceEnd
		return s.cursor
	}
	s.base.setText(s.text[s.sliceStart:s.sliceEnd])
	s.cursor = s.sliceStart + s.base.last()
	return s.cursor
}

func (s *splittingBreakIterator) current() int { return s.cursor }

func (s *splittingBreakIterator) next() int { return s.following(s.cursor) }

func (s *splittingBreakIterator) following(offset int) int {
	if offset+1 < s.sliceStart || offset+1 > s.sliceEnd {
		if offset == len(s.text) {
			s.last()
			return breakDone
		}
		s.sliceStart = lastIndexOfUnit(s.text, s.sep, offset) + 1
		s.sliceEnd = indexOfUnit(s.text, s.sep, max(offset+1, s.sliceStart))
		if s.sliceEnd == -1 {
			s.sliceEnd = len(s.text)
		}
		if s.sliceStart != s.sliceEnd {
			s.base.setText(s.text[s.sliceStart:s.sliceEnd])
		}
	}
	switch {
	case s.sliceStart == s.sliceEnd:
		s.cursor = offset + 1
	case offset == s.sliceStart-1:
		s.cursor = s.sliceStart + s.base.first()
	default:
		s.cursor = s.sliceStart + s.base.following(offset-s.sliceStart)
	}
	return s.cursor
}

func (s *splittingBreakIterator) preceding(offset int) int {
	if offset-1 < s.sliceStart || offset-1 > s.sliceEnd {
		if offset == 0 {
			s.first()
			return breakDone
		}
		s.sliceEnd = indexOfUnit(s.text, s.sep, offset)
		if s.sliceEnd == -1 {
			s.sliceEnd = len(s.text)
		}
		s.sliceStart = lastIndexOfUnit(s.text, s.sep, offset-1)
		if s.sliceStart == -1 {
			s.sliceStart = 0
		} else {
			s.sliceStart = min(s.sliceStart+1, s.sliceEnd)
		}
		if s.sliceStart != s.sliceEnd {
			s.base.setText(s.text[s.sliceStart:s.sliceEnd])
		}
	}
	switch {
	case s.sliceStart == s.sliceEnd:
		s.cursor = offset - 1
	case offset == s.sliceEnd+1:
		s.cursor = s.sliceStart + s.base.last()
	default:
		s.cursor = s.sliceStart + s.base.preceding(offset-s.sliceStart)
	}
	return s.cursor
}

// passages ------------------------------------------------------------------

// hlMatch is a highlighted token: label is the term, or the multi-term
// query for tokens matched by one, and freq the frequency used to score.
type hlMatch struct {
	start, end int
	label      string
	freq       int
	boost      float64
}

type hlPassage struct {
	start, end int
	score      float32
	matches    []hlMatch
}

// hlUnifiedMatches marks the tokens matching the query parts, ordered by
// start offset and label.
func hlUnifiedMatches(toks []hlToken, qt *hlQueryTerms) []hlMatch {
	freq := map[string]int{}
	for _, tok := range toks {
		freq[tok.term]++
	}
	insensitive := map[string]bool{}
	for _, term := range qt.terms {
		insensitive[term.text] = true
	}
	var ms []hlMatch
	for _, tok := range toks {
		if insensitive[tok.term] {
			ms = append(ms, hlMatch{start: tok.start, end: tok.end, label: tok.term, freq: freq[tok.term]})
		}
	}
	for _, a := range qt.automata {
		sum := 0
		for term, n := range freq {
			if a.match(term) {
				sum += n
			}
		}
		if sum == 0 {
			continue
		}
		for _, tok := range toks {
			if a.match(tok.term) {
				ms = append(ms, hlMatch{start: tok.start, end: tok.end, label: a.label, freq: sum})
			}
		}
	}
	// phrase terms score with the number of positions collected for them
	type spanKey struct {
		term       string
		start, end int
	}
	seen := map[spanKey]bool{}
	pairs := map[string]int{}
	var collected []hlToken
	for i := range qt.spans {
		matched, _ := hlSpanMatches(&qt.spans[i], toks)
		idx := make([]int, 0, len(matched))
		for ti := range matched {
			idx = append(idx, ti)
		}
		sort.Ints(idx)
		for _, ti := range idx {
			tok := toks[ti]
			k := spanKey{tok.term, tok.start, tok.end}
			if insensitive[tok.term] || seen[k] {
				continue
			}
			seen[k] = true
			pairs[tok.term]++
			collected = append(collected, tok)
		}
	}
	for _, tok := range collected {
		ms = append(ms, hlMatch{start: tok.start, end: tok.end, label: tok.term, freq: pairs[tok.term]})
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].start != ms[j].start {
			return ms[i].start < ms[j].start
		}
		return ms[i].label < ms[j].label
	})
	return ms
}

// hlPassageScore is Lucene's PassageScorer with k1 1.2, b 0.75 and pivot
// 87, computed in float32 like Java. Conversions keep the Go compiler from
// fusing multiply-add operations.
func hlPassageScore(p *hlPassage, contentLength int) float32 {
	const k1, b, pivot = float32(1.2), float32(0.75), float32(87)
	var labels []string
	inPassage := map[string]int{}
	inDoc := map[string]int{}
	for _, m := range p.matches {
		if _, ok := inPassage[m.label]; !ok {
			labels = append(labels, m.label)
			inDoc[m.label] = m.freq
		}
		inPassage[m.label]++
	}
	length := float32(p.end - p.start)
	numDocs := 1 + float32(float32(contentLength)/pivot)
	var score float32
	for _, label := range labels {
		freq := float32(inPassage[label])
		norm := float32(k1 * float32((1-b)+float32(b*float32(length/pivot))))
		tf := float32(freq / float32(freq+norm))
		weight := float32((k1 + 1) * float32(math.Log(1+(float64(numDocs)+0.5)/(float64(inDoc[label])+0.5))))
		score = score + float32(tf*weight)
	}
	norm := 1 + 1/float32(math.Log(float64(pivot+float32(p.start))))
	return float32(score * norm)
}

// hlBuildPassages is OpenSearch's CustomFieldHighlighter.highlightOffsetsEnums:
// a passage covers the break iterator boundaries around its first match and
// the matches before its end; the best maxPassages passages are kept.
func hlBuildPassages(bi breakIterator, ms []hlMatch, contentLength, maxPassages int) []*hlPassage {
	var queue []*hlPassage
	minIndex := func() int {
		m := 0
		for i, p := range queue {
			if p.score < queue[m].score || p.score == queue[m].score && p.start < queue[m].start {
				m = i
			}
		}
		return m
	}
	add := func(p *hlPassage) *hlPassage {
		if p.start == -1 {
			return p
		}
		p.score = hlPassageScore(p, contentLength)
		if len(queue) == maxPassages && p.score < queue[minIndex()].score {
			return &hlPassage{start: -1, end: -1}
		}
		queue = append(queue, p)
		if len(queue) > maxPassages {
			m := minIndex()
			queue = append(queue[:m], queue[m+1:]...)
		}
		return &hlPassage{start: -1, end: -1}
	}
	cur := &hlPassage{start: -1, end: -1}
	for _, m := range ms {
		if m.start < contentLength && m.end > contentLength {
			continue
		}
		if m.start >= cur.end {
			cur = add(cur)
			if m.start >= contentLength {
				break
			}
			cur.start = max(bi.preceding(m.start+1), 0)
			cur.end = min(bi.following(m.start), contentLength)
		}
		cur.matches = append(cur.matches, m)
	}
	add(cur)
	sort.Slice(queue, func(i, j int) bool { return queue[i].start < queue[j].start })
	return queue
}

// javaTrim is String.trim.
func javaTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

// javaHasText is Strings.hasText: some character is not Java whitespace.
func javaHasText(s string) bool {
	for _, r := range s {
		switch {
		case r == '\t', r == '\n', r == '\v', r == '\f', r == '\r', r >= 0x1c && r <= 0x1f:
		case r == 0x00a0 || r == 0x2007 || r == 0x202f:
			return true
		case unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp):
		default:
			return true
		}
	}
	return false
}

// hlFormatPassage is OpenSearch's CustomPassageFormatter: overlapping
// matches are merged, the separator ending a value is dropped and the
// snippet is trimmed.
func hlFormatPassage(p *hlPassage, content []uint16, o *hlFieldOptions) string {
	var b strings.Builder
	text := func(from, to int) string {
		if to <= from {
			return ""
		}
		return o.encode(string(utf16.Decode(content[from:to])))
	}
	pos := p.start
	for i := 0; i < len(p.matches); i++ {
		start := p.matches[i].start
		b.WriteString(text(pos, start))
		end := p.matches[i].end
		for i+1 < len(p.matches) && p.matches[i+1].start < end {
			i++
			end = max(end, p.matches[i].end)
		}
		end = min(end, p.end)
		b.WriteString(o.preTag(0))
		b.WriteString(text(start, end))
		b.WriteString(o.postTag(0))
		pos = end
	}
	b.WriteString(text(pos, max(pos, p.end)))
	s := b.String()
	if strings.HasSuffix(s, " ") {
		s = strings.TrimSuffix(s, " ")
	} else if strings.HasSuffix(s, "\x00") {
		s = strings.TrimSuffix(s, "\x00")
	}
	return javaTrim(s)
}

// unifiedHighlight highlights one field of a hit.
func unifiedHighlight(h *hit, t *hlTarget, all *hlQueryTerms) ([]string, error) {
	o := &t.opts
	if o.maxAnalyzerOffset != nil && *o.maxAnalyzerOffset <= 0 {
		return nil, errSearchPhase(errIllegalArgument("the value [%d] of max_analyzer_offset is invalid", *o.maxAnalyzerOffset))
	}
	tokenized := t.field.Type == TypeText || t.field.Type == TypeMatchOnlyText || t.field.Type == TypeSearchAsYouType
	maxPassages := o.numberOfFragments
	var base breakIterator
	if o.numberOfFragments == 0 || !tokenized {
		base = &separatorBreakIterator{sep: 0}
		if o.numberOfFragments == 0 {
			maxPassages = math.MaxInt32 - 1
		}
	} else {
		switch o.boundaryScanner {
		case "", "sentence":
			if o.fragmentSize > 0 {
				base = newBoundedBreakIterator(o.fragmentSize)
			} else {
				base = newSentenceBreakIterator()
			}
		case "word":
			base = newWordBreakIterator()
		default:
			return nil, errSearchPhase(errIllegalArgument("Invalid boundary scanner type: %s", o.boundaryScanner))
		}
	}
	qt := all.forField(h.ix, t)
	if qt.empty() && o.noMatchSize == 0 {
		return nil, nil
	}
	values := hlFieldValues(h, t)
	if len(values) == 0 {
		return nil, nil
	}
	content, starts := hlValuesText(values, 0)
	if len(content) == 0 {
		return nil, nil
	}
	bi := &splittingBreakIterator{base: base, sep: 0}
	bi.setText(content)
	var passages []*hlPassage
	if !qt.empty() {
		ms := hlUnifiedMatches(hlAnalyze(h.ix, t.field, values, starts, o.maxAnalyzerOffset), qt)
		if len(ms) > 0 && maxPassages+1 < 1 {
			return nil, errSearchPhase(errIllegalArgument(""))
		}
		passages = hlBuildPassages(bi, ms, len(content), maxPassages)
	}
	if len(passages) == 0 && o.noMatchSize > 0 {
		pos := 0
		for pos < len(content) && content[pos] == 0 {
			pos++
		}
		if pos < len(content) {
			end := indexOfUnit(content, 0, pos)
			if end == -1 {
				end = len(content)
			}
			if o.noMatchSize+pos < end {
				wb := newWordBreakIterator()
				wb.setText(content)
				if end = wb.following(o.noMatchSize + pos); end == breakDone {
					end = len(content)
				}
			}
			passages = []*hlPassage{{start: pos, end: end, score: float32(math.NaN())}}
		}
	}
	type snippet struct {
		text  string
		score float32
	}
	var snippets []snippet
	for _, p := range passages {
		if s := hlFormatPassage(p, content, o); javaHasText(s) {
			snippets = append(snippets, snippet{s, p.score})
		}
	}
	if o.scoreOrdered {
		sort.SliceStable(snippets, func(i, j int) bool {
			a, b := snippets[i].score, snippets[j].score
			if math.IsNaN(float64(a)) || math.IsNaN(float64(b)) {
				return math.IsNaN(float64(a)) && !math.IsNaN(float64(b))
			}
			return a > b
		})
	}
	out := make([]string, len(snippets))
	for i, s := range snippets {
		out[i] = s.text
	}
	return out, nil
}
