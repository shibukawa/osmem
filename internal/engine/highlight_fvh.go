package engine

import (
	"math"
	"sort"
	"strings"
	"unicode/utf16"
)

// The fast vector highlighter (type fvh) works on the term vectors of
// fields indexed with term_vector with_positions_offsets. It is Lucene's
// FastVectorHighlighter as OpenSearch configures it: the query is
// flattened into terms and phrases (multi-term queries expand to the
// matching terms of the index), consecutive query terms in the text are
// joined into phrases and highlighted as one match, fragments of
// fragment_size characters are centered on the matches and widened to the
// next boundary characters (or word / sentence breaks), and every query
// term or phrase gets its own tag when several pre_tags are given.

// fvhFlat is a flattened query: a term, or a phrase of one term per
// position.
type fvhFlat struct {
	field string
	terms []string
	slop  int
	boost float32
}

// fvhPhraseMap is FieldQuery.QueryPhraseMap.
type fvhPhraseMap struct {
	terminal bool
	slop     int
	boost    float32
	number   int
	sub      map[string]*fvhPhraseMap
}

func (m *fvhPhraseMap) child(term string) *fvhPhraseMap {
	if m.sub == nil {
		m.sub = map[string]*fvhPhraseMap{}
	}
	c := m.sub[term]
	if c == nil {
		c = &fvhPhraseMap{}
		m.sub[term] = c
	}
	return c
}

func (m *fvhPhraseMap) validFor(candidate []*fvhTermInfo) bool {
	if !m.terminal {
		return false
	}
	if len(candidate) == 1 {
		return true
	}
	pos := candidate[0].pos
	for _, ti := range candidate[1:] {
		d := ti.pos - pos - 1
		if d < 0 {
			d = -d
		}
		if d > m.slop {
			return false
		}
		pos = ti.pos
	}
	return true
}

// fvhFieldQuery is FieldQuery: phrase maps and term sets per field (or one
// for all fields when require_field_match is false).
type fvhFieldQuery struct {
	fieldMatch bool
	roots      map[string]*fvhPhraseMap
	termSets   map[string]map[string]bool
	next       int
}

func (fq *fvhFieldQuery) key(field string) string {
	if fq.fieldMatch {
		return field
	}
	return ""
}

func (fq *fvhFieldQuery) add(f fvhFlat) {
	k := fq.key(f.field)
	root := fq.roots[k]
	if root == nil {
		root = &fvhPhraseMap{}
		fq.roots[k] = root
	}
	m := root
	for _, term := range f.terms {
		m = m.child(term)
	}
	m.terminal, m.slop, m.boost, m.number = true, f.slop, f.boost, fq.next
	fq.next++
	set := fq.termSets[k]
	if set == nil {
		set = map[string]bool{}
		fq.termSets[k] = set
	}
	for _, term := range f.terms {
		set[term] = true
	}
}

func (fq *fvhFieldQuery) termMap(field, term string) *fvhPhraseMap {
	if root := fq.roots[fq.key(field)]; root != nil {
		return root.sub[term]
	}
	return nil
}

func (fq *fvhFieldQuery) searchPhrase(field string, candidate []*fvhTermInfo) *fvhPhraseMap {
	m := fq.roots[fq.key(field)]
	if m == nil {
		return nil
	}
	for _, ti := range candidate {
		if m = m.sub[ti.text]; m == nil {
			return nil
		}
	}
	if m.validFor(candidate) {
		return m
	}
	return nil
}

// fvhIndexTerms lists the terms of a field in the index that match a
// multi-term query (at most 1024, like the rewrite FieldQuery uses).
func fvhIndexTerms(ix *Index, a *hlAutomaton) []fvhFlat {
	dict, err := ix.bleve.FieldDict(a.field)
	if err != nil {
		return nil
	}
	defer dict.Close()
	var out []fvhFlat
	for {
		entry, err := dict.Next()
		if err != nil || entry == nil {
			break
		}
		if !a.match(entry.Term) {
			continue
		}
		boost := float32(1)
		if a.fuzzy != nil {
			term := []rune(entry.Term)
			for ed := 0; ed <= 2; ed++ {
				if fuzzyMatch(a.fuzzy, term, 0, ed) {
					if ed > 0 {
						boost = 1 - float32(ed)/float32(min(len(term), len(a.fuzzy)))
					}
					break
				}
			}
		}
		if w, ok := a.weights[entry.Term]; ok {
			boost = float32(w)
		}
		out = append(out, fvhFlat{field: a.field, terms: []string{entry.Term}, boost: float32(a.boost) * boost})
	}
	if len(out) > 1024 {
		sort.SliceStable(out, func(i, j int) bool { return out[i].boost > out[j].boost })
		out = out[:1024]
		sort.SliceStable(out, func(i, j int) bool { return out[i].terms[0] < out[j].terms[0] })
	}
	return out
}

// fvhBuildFieldQuery flattens the query parts in query order.
func fvhBuildFieldQuery(ix *Index, qt *hlQueryTerms, fieldMatch bool) *fvhFieldQuery {
	type part struct {
		seq   int
		flats []fvhFlat
	}
	stringField := func(field string) (string, bool) {
		f, full, _ := hlResolveField(ix.Mapping, field)
		if f == nil || !(f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeKeyword || f.Type == TypeSearchAsYouType) {
			return "", false
		}
		return full, true
	}
	var parts []part
	for _, t := range qt.terms {
		if full, ok := stringField(t.field); ok {
			parts = append(parts, part{t.seq, []fvhFlat{{field: full, terms: []string{t.text}, boost: float32(t.boost)}}})
		}
	}
	for i := range qt.automata {
		a := &qt.automata[i]
		if full, ok := stringField(a.field); ok {
			flats := fvhIndexTerms(ix, a)
			for j := range flats {
				flats[j].field = full
			}
			parts = append(parts, part{a.seq, flats})
		}
	}
	for _, sp := range qt.spans {
		full, ok := stringField(sp.field)
		if !ok {
			continue
		}
		// positions: a prefix expands to the matching terms of the index
		positions := make([][]string, len(sp.clauses))
		for c, clause := range sp.clauses {
			if clause.prefix == nil {
				positions[c] = clause.terms
				continue
			}
			prefix := *clause.prefix
			for _, f := range fvhIndexTerms(ix, &hlAutomaton{field: sp.field, match: func(term string) bool { return strings.HasPrefix(term, prefix) }}) {
				if len(positions[c]) >= 50 {
					break
				}
				positions[c] = append(positions[c], f.terms[0])
			}
		}
		var flats []fvhFlat
		switch {
		case !sp.multi:
			terms := make([]string, len(positions))
			for c, ts := range positions {
				terms[c] = ts[0]
			}
			flats = append(flats, fvhFlat{field: full, terms: terms, slop: sp.slop, boost: float32(sp.boost)})
		default:
			total := 0
			for _, ts := range positions {
				total += len(ts)
			}
			if total > 16 {
				for _, ts := range positions {
					for _, term := range ts {
						flats = append(flats, fvhFlat{field: full, terms: []string{term}, boost: 1})
					}
				}
				break
			}
			var walk func(c int, acc []string)
			walk = func(c int, acc []string) {
				if c == len(positions) {
					flats = append(flats, fvhFlat{field: full, terms: append([]string(nil), acc...), slop: sp.slop, boost: 1})
					return
				}
				for _, term := range positions[c] {
					walk(c+1, append(acc, term))
				}
			}
			walk(0, nil)
		}
		parts = append(parts, part{sp.seq, flats})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].seq < parts[j].seq })
	fq := &fvhFieldQuery{fieldMatch: fieldMatch, roots: map[string]*fvhPhraseMap{}, termSets: map[string]map[string]bool{}}
	seen := map[string]bool{}
	for _, p := range parts {
		for _, f := range p.flats {
			if len(f.terms) == 0 {
				continue
			}
			key := f.field + "\x00" + strings.Join(f.terms, "\x01") + "\x00" + string(rune(f.slop)) + "\x00" + string(rune(math.Float32bits(f.boost)))
			if seen[key] {
				continue
			}
			seen[key] = true
			fq.add(f)
		}
	}
	return fq
}

// fvhTermInfo is FieldTermStack.TermInfo; tokens at the same position are
// chained through next.
type fvhTermInfo struct {
	text       string
	start, end int
	pos        int
	next       *fvhTermInfo
}

type fvhToffs struct{ start, end int }

// fvhPhrase is FieldPhraseList.WeightedPhraseInfo.
type fvhPhrase struct {
	terms   []*fvhTermInfo
	offsets []*fvhToffs
	boost   float32
	seq     int
}

func newFvhPhrase(terms []*fvhTermInfo, boost float32, seq int) *fvhPhrase {
	p := &fvhPhrase{terms: append([]*fvhTermInfo(nil), terms...), boost: boost, seq: seq}
	p.offsets = []*fvhToffs{{terms[0].start, terms[0].end}}
	pos := terms[0].pos
	for _, ti := range terms[1:] {
		if ti.pos-pos == 1 {
			p.offsets[len(p.offsets)-1].end = ti.end
		} else {
			p.offsets = append(p.offsets, &fvhToffs{ti.start, ti.end})
		}
		pos = ti.pos
	}
	return p
}

func (p *fvhPhrase) start() int { return p.offsets[0].start }
func (p *fvhPhrase) end() int   { return p.offsets[len(p.offsets)-1].end }

func (p *fvhPhrase) overlaps(o *fvhPhrase) bool {
	so, eo, oso, oeo := p.start(), p.end(), o.start(), o.end()
	return so <= oso && oso < eo || so < oeo && oeo <= eo || oso <= so && so < oeo || oso < eo && eo <= oeo
}

// fvhTermStack is FieldTermStack: the tokens of the field that are query
// terms, by position.
func fvhTermStack(ix *Index, fq *fvhFieldQuery, h *hit, field string) []*fvhTermInfo {
	set := fq.termSets[fq.key(field)]
	if set == nil {
		return nil
	}
	f, _, source := hlResolveField(ix.Mapping, field)
	if f == nil {
		return nil
	}
	values := hlFieldValues(h, &hlTarget{field: f, source: source})
	_, starts := hlValuesText(values, ' ')
	var list []*fvhTermInfo
	for _, tok := range hlAnalyze(ix, f, values, starts, nil) {
		if set[tok.term] {
			list = append(list, &fvhTermInfo{text: tok.term, start: tok.start, end: tok.end, pos: tok.pos})
		}
	}
	// term vectors list the terms in order, then positions are sorted
	sort.SliceStable(list, func(i, j int) bool { return list[i].text < list[j].text })
	sort.SliceStable(list, func(i, j int) bool { return list[i].pos < list[j].pos })
	var out []*fvhTermInfo
	var first, previous *fvhTermInfo
	for _, ti := range list {
		if previous != nil && ti.pos == previous.pos {
			previous.next = ti
			previous = ti
			continue
		}
		if previous != nil {
			previous.next = first
		}
		previous, first = ti, ti
		out = append(out, ti)
	}
	if previous != nil {
		previous.next = first
	}
	return out
}

// fvhPhraseList is FieldPhraseList.
func fvhPhraseList(stack []*fvhTermInfo, fq *fvhFieldQuery, field string, phraseLimit int) []*fvhPhrase {
	var phrases []*fvhPhrase
	pop := func() *fvhTermInfo {
		if len(stack) == 0 {
			return nil
		}
		ti := stack[0]
		stack = stack[1:]
		return ti
	}
	push := func(ti *fvhTermInfo) { stack = append([]*fvhTermInfo{ti}, stack...) }
	addIfNoOverlap := func(p *fvhPhrase) {
		for _, existing := range phrases {
			if existing.overlaps(p) {
				existing.terms = append(existing.terms, p.terms...)
				return
			}
		}
		phrases = append(phrases, p)
	}
	for len(stack) > 0 && len(phrases) < phraseLimit {
		var candidate []*fvhTermInfo
		ti := pop()
		first := ti
		curr := fq.termMap(field, ti.text)
		for curr == nil && ti.next != first {
			ti = ti.next
			curr = fq.termMap(field, ti.text)
		}
		if curr == nil {
			continue
		}
		candidate = append(candidate, ti)
		for {
			ti = pop()
			first = ti
			var nextMap *fvhPhraseMap
			if ti != nil {
				nextMap = curr.sub[ti.text]
				for nextMap == nil && ti.next != first {
					ti = ti.next
					nextMap = curr.sub[ti.text]
				}
			}
			if ti == nil || nextMap == nil {
				if ti != nil {
					push(ti)
				}
				if curr.validFor(candidate) {
					addIfNoOverlap(newFvhPhrase(candidate, curr.boost, curr.number))
				} else {
					for len(candidate) > 1 {
						push(candidate[len(candidate)-1])
						candidate = candidate[:len(candidate)-1]
						if m := fq.searchPhrase(field, candidate); m != nil {
							addIfNoOverlap(newFvhPhrase(candidate, m.boost, m.number))
							break
						}
					}
				}
				break
			}
			candidate = append(candidate, ti)
			curr = nextMap
		}
	}
	return phrases
}

// fvhMergePhraseLists merges the phrase lists of matched_fields.
func fvhMergePhraseLists(lists [][]*fvhPhrase) []*fvhPhrase {
	var all []*fvhPhrase
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.start() != b.start() {
			return a.start() < b.start()
		}
		if a.end() != b.end() {
			return a.end() < b.end()
		}
		return a.boost < b.boost
	})
	var out []*fvhPhrase
	var work []*fvhPhrase
	workEnd := 0
	flush := func() {
		if len(work) == 1 {
			out = append(out, work[0])
			return
		}
		merged := &fvhPhrase{seq: work[0].seq, boost: work[0].boost}
		var toffs []*fvhToffs
		for i, p := range work {
			if i > 0 {
				merged.boost += p.boost
			}
			merged.terms = append(merged.terms, p.terms...)
			for _, t := range p.offsets {
				toffs = append(toffs, &fvhToffs{t.start, t.end})
			}
		}
		sort.SliceStable(toffs, func(i, j int) bool {
			if toffs[i].start != toffs[j].start {
				return toffs[i].start < toffs[j].start
			}
			return toffs[i].end < toffs[j].end
		})
		cur := toffs[0]
		for _, t := range toffs[1:] {
			if t.start <= cur.end {
				cur.end = max(cur.end, t.end)
			} else {
				merged.offsets = append(merged.offsets, cur)
				cur = t
			}
		}
		merged.offsets = append(merged.offsets, cur)
		out = append(out, merged)
	}
	for i, p := range all {
		if i == 0 {
			work, workEnd = []*fvhPhrase{p}, p.end()
			continue
		}
		if p.start() <= workEnd {
			workEnd = max(workEnd, p.end())
			work = append(work, p)
			continue
		}
		flush()
		work, workEnd = []*fvhPhrase{p}, p.end()
	}
	if len(work) > 0 {
		flush()
	}
	return out
}

type fvhSubInfo struct {
	offsets []*fvhToffs
	seq     int
	boost   float32
}

type fvhFragInfo struct {
	start, end int
	subs       []*fvhSubInfo
	boost      float32
}

func newFvhFragInfo(start, end int, phrases []*fvhPhrase) *fvhFragInfo {
	fi := &fvhFragInfo{start: start, end: end}
	for _, p := range phrases {
		offsets := make([]*fvhToffs, len(p.offsets))
		for i, t := range p.offsets {
			offsets[i] = &fvhToffs{t.start, t.end}
		}
		fi.subs = append(fi.subs, &fvhSubInfo{offsets: offsets, seq: p.seq, boost: p.boost})
		fi.boost += p.boost
	}
	return fi
}

// fvhFragList is SimpleFragListBuilder: fragments of fragCharSize centered
// on the matches they contain.
func fvhFragList(phrases []*fvhPhrase, fragCharSize, margin int) ([]*fvhFragInfo, *Error) {
	minFragCharSize := max(1, margin*3)
	if fragCharSize < minFragCharSize {
		return nil, errSearchPhase(errIllegalArgument("fragCharSize(%d) is too small. It must be %d or higher.", fragCharSize, minFragCharSize))
	}
	accept := func(p *fvhPhrase, matchLen int) bool { return len(p.offsets) <= 1 || matchLen <= fragCharSize }
	var frags []*fvhFragInfo
	startOffset := 0
	for i := 0; i < len(phrases); {
		p := phrases[i]
		if p.start() < startOffset {
			i++
			continue
		}
		var wpil []*fvhPhrase
		curStart, curEnd := p.start(), p.end()
		spanStart := max(curStart-margin, startOffset)
		spanEnd := max(curEnd, spanStart+fragCharSize)
		i++
		if accept(p, curEnd-curStart) {
			wpil = append(wpil, p)
		}
		for i < len(phrases) && phrases[i].end() <= spanEnd {
			p = phrases[i]
			curEnd = p.end()
			i++
			if accept(p, curEnd-curStart) {
				wpil = append(wpil, p)
			}
		}
		if len(wpil) == 0 {
			continue
		}
		matchLen := curEnd - curStart
		newMargin := max(0, (fragCharSize-matchLen)/2)
		spanStart = max(curStart-newMargin, startOffset)
		spanEnd = spanStart + max(matchLen, fragCharSize)
		startOffset = spanEnd
		frags = append(frags, newFvhFragInfo(spanStart, spanEnd, wpil))
	}
	return frags, nil
}

// fvhBoundaryScanner widens fragments to boundaries.
type fvhBoundaryScanner interface {
	findStart(buf []uint16, start int) int
	findEnd(buf []uint16, start int) int
}

type fvhCharsScanner struct {
	maxScan int
	chars   string
}

func (s *fvhCharsScanner) boundary(c uint16) bool {
	return strings.ContainsRune(s.chars, rune(c))
}

func (s *fvhCharsScanner) findStart(buf []uint16, start int) int {
	if start > len(buf) || start < 1 {
		return start
	}
	offset := start
	for count := s.maxScan; offset > 0 && count > 0; count-- {
		if s.boundary(buf[offset-1]) {
			return offset
		}
		offset--
	}
	if offset == 0 {
		return 0
	}
	return start
}

func (s *fvhCharsScanner) findEnd(buf []uint16, start int) int {
	if start > len(buf) || start < 0 {
		return start
	}
	offset := start
	for count := s.maxScan; offset < len(buf) && count > 0; count-- {
		if s.boundary(buf[offset]) {
			return offset
		}
		offset++
	}
	return start
}

type fvhBreakScanner struct {
	bi *ruleBreakIterator
}

func (s *fvhBreakScanner) findStart(buf []uint16, start int) int {
	if start > len(buf) || start < 1 {
		return start
	}
	s.bi.setText(buf[:start])
	s.bi.last()
	return s.bi.previous()
}

func (s *fvhBreakScanner) findEnd(buf []uint16, start int) int {
	if start < 0 {
		return start
	}
	s.bi.setText(buf[start:])
	return s.bi.next() + start
}

// fvhDiscreteValues is BaseFragmentsBuilder.discreteMultiValueHighlighting:
// fragments are cut at value boundaries.
func fvhDiscreteValues(frags []*fvhFragInfo, values [][]uint16) []*fvhFragInfo {
	var result []*fvhFragInfo
fragLoop:
	for _, fi := range frags {
		fieldEnd := 0
		for _, v := range values {
			if len(v) == 0 {
				fieldEnd++
				continue
			}
			fieldStart := fieldEnd
			fieldEnd += len(v) + 1
			if fi.start >= fieldStart && fi.end >= fieldStart && fi.start <= fieldEnd && fi.end <= fieldEnd {
				result = append(result, fi)
				continue fragLoop
			}
			if len(fi.subs) == 0 {
				continue fragLoop
			}
			first := fi.subs[0].offsets[0]
			if fi.start >= fieldEnd || first.start >= fieldEnd {
				continue
			}
			fragStart := fieldStart
			if fi.start > fieldStart && fi.start < fieldEnd {
				fragStart = fi.start
			}
			fragEnd := fieldEnd
			if fi.end > fieldStart && fi.end < fieldEnd {
				fragEnd = fi.end
			}
			var subs []*fvhSubInfo
			var boost float32
			keptSubs := fi.subs[:0]
			for _, sub := range fi.subs {
				var toffs []*fvhToffs
				remaining := sub.offsets[:0]
				for k := 0; k < len(sub.offsets); k++ {
					t := sub.offsets[k]
					if t.start >= fieldEnd {
						remaining = append(remaining, sub.offsets[k:]...)
						break
					}
					startsAfter := t.start >= fieldStart
					endsBefore := t.end < fieldEnd
					switch {
					case startsAfter && endsBefore:
						toffs = append(toffs, t)
					case startsAfter:
						toffs = append(toffs, &fvhToffs{t.start, fieldEnd - 1})
						remaining = append(remaining, t)
					case endsBefore:
						toffs = append(toffs, &fvhToffs{fieldStart, t.end})
					default:
						toffs = append(toffs, &fvhToffs{fieldStart, fieldEnd - 1})
						remaining = append(remaining, t)
					}
				}
				sub.offsets = remaining
				if len(toffs) > 0 {
					subs = append(subs, &fvhSubInfo{offsets: toffs, seq: sub.seq, boost: sub.boost})
					boost += sub.boost
				}
				if len(sub.offsets) > 0 {
					keptSubs = append(keptSubs, sub)
				}
			}
			fi.subs = keptSubs
			result = append(result, &fvhFragInfo{start: fragStart, end: fragEnd, subs: subs, boost: boost})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].start < result[j].start })
	return result
}

// fvhCreateFragments is BaseFragmentsBuilder.createFragments over the source
// values.
func fvhCreateFragments(values []string, tokenized bool, frags []*fvhFragInfo, maxNum int, o *hlFieldOptions, scanner fvhBoundaryScanner) []string {
	if len(values) == 0 {
		return nil
	}
	encoded := make([][]uint16, len(values))
	for i, v := range values {
		encoded[i] = utf16.Encode([]rune(v))
	}
	if len(values) > 1 {
		frags = fvhDiscreteValues(frags, encoded)
	}
	// number_of_fragments 0 uses the simple fragments builder, never ordered by score
	if o.scoreOrdered && o.numberOfFragments != 0 {
		sort.SliceStable(frags, func(i, j int) bool {
			if frags[i].boost != frags[j].boost {
				return frags[i].boost > frags[j].boost
			}
			return frags[i].start < frags[j].start
		})
	}
	limit := min(maxNum, len(frags))
	var buffer []uint16
	next := 0
	out := make([]string, 0, limit)
	text := func(src []uint16, from, to int) string {
		from, to = max(0, min(from, len(src))), max(0, min(to, len(src)))
		if to <= from {
			return ""
		}
		return o.encode(string(utf16.Decode(src[from:to])))
	}
	for n := 0; n < limit; n++ {
		fi := frags[n]
		for len(buffer) < fi.end && next < len(encoded) {
			buffer = append(buffer, encoded[next]...)
			buffer = append(buffer, ' ')
			next++
		}
		bufferLength := len(buffer)
		if tokenized {
			bufferLength--
		}
		eo := bufferLength
		if bufferLength >= fi.end {
			eo = scanner.findEnd(buffer, fi.end)
		}
		mso := scanner.findStart(buffer, fi.start)
		src := buffer[max(0, min(mso, len(buffer))):max(0, min(eo, len(buffer)))]
		var b strings.Builder
		srcIndex := 0
		for _, sub := range fi.subs {
			for _, t := range sub.offsets {
				b.WriteString(text(src, srcIndex, t.start-mso))
				b.WriteString(o.preTag(sub.seq))
				b.WriteString(text(src, t.start-mso, t.end-mso))
				b.WriteString(o.postTag(sub.seq))
				srcIndex = t.end - mso
			}
		}
		b.WriteString(text(src, srcIndex, len(src)))
		out = append(out, b.String())
	}
	return out
}

// fvhHighlight highlights one field of a hit with the fast vector
// highlighter.
func fvhHighlight(h *hit, t *hlTarget, all *hlQueryTerms) ([]string, error) {
	o := &t.opts
	if !hlTermVectorsWithOffsets(t.field) {
		return nil, errSearchPhase(errIllegalArgument("the field [%s] should be indexed with term vector with position offsets to be used with fast vector highlighter", t.full))
	}
	var scanner fvhBoundaryScanner
	switch o.boundaryScanner {
	case "sentence":
		scanner = &fvhBreakScanner{bi: newSentenceBreakIterator()}
	case "word":
		scanner = &fvhBreakScanner{bi: newWordBreakIterator()}
	default:
		scanner = &fvhCharsScanner{maxScan: o.boundaryMaxScan, chars: o.boundaryChars}
	}
	margin := 6
	if o.numberOfFragments != 0 && o.fragmentOffset != -1 {
		if o.fragmentOffset < 0 {
			return nil, errSearchPhase(errIllegalArgument("margin(%d) is too small. It must be 0 or higher.", o.fragmentOffset))
		}
		margin = o.fragmentOffset
	}
	fq := fvhBuildFieldQuery(h.ix, all, o.requireFieldMatch)
	fragCharSize, maxNum := o.fragmentSize, o.numberOfFragments
	if o.numberOfFragments == 0 {
		fragCharSize, maxNum = math.MaxInt32, math.MaxInt32
	}
	matched := []string{t.full}
	if len(o.matchedFields) > 0 {
		matched = o.matchedFields
	}
	var phrases []*fvhPhrase
	if len(matched) == 1 {
		phrases = fvhPhraseList(fvhTermStack(h.ix, fq, h, matched[0]), fq, matched[0], o.phraseLimit)
	} else {
		lists := make([][]*fvhPhrase, 0, len(matched))
		for _, mf := range matched {
			lists = append(lists, fvhPhraseList(fvhTermStack(h.ix, fq, h, mf), fq, mf, o.phraseLimit))
		}
		phrases = fvhMergePhraseLists(lists)
	}
	var frags []*fvhFragInfo
	if o.numberOfFragments == 0 {
		if len(phrases) > 0 {
			frags = []*fvhFragInfo{newFvhFragInfo(0, math.MaxInt32, phrases)}
		}
	} else {
		var err *Error
		if frags, err = fvhFragList(phrases, fragCharSize, margin); err != nil {
			return nil, err
		}
	}
	if maxNum < 0 {
		return nil, errSearchPhase(errIllegalArgument("maxNumFragments(%d) must be positive number.", maxNum))
	}
	values := hlFieldValues(h, t)
	tokenized := t.field.Type == TypeText || t.field.Type == TypeMatchOnlyText || t.field.Type == TypeSearchAsYouType
	if fragments := fvhCreateFragments(values, tokenized, frags, maxNum, o, scanner); len(fragments) > 0 {
		return fragments, nil
	}
	if o.noMatchSize > 0 {
		return fvhCreateFragments(values, tokenized, []*fvhFragInfo{{start: 0, end: o.noMatchSize}}, 1, o, scanner), nil
	}
	return nil, nil
}
