package engine

import (
	"sort"
	"strings"
	"unicode/utf16"
)

// The plain highlighter is Lucene's Highlighter as OpenSearch configures it:
// every value is analyzed on its own, the query terms are weighted by their
// boosts (phrase terms only count inside the phrase matches), fragments are
// cut by the span fragmenter (a new fragment starts at the first token ending
// at or after the next multiple of fragment_size, unless that leaves less
// than half a fragment or a phrase is still open), the simple fragmenter, or
// not at all for number_of_fragments 0. Fragments are ranked by the sum of
// the weights of the distinct terms they contain and are not trimmed.

// plainWeightedTerm is a WeightedSpanTerm: phrase terms are position
// sensitive and only score inside their match ranges.
type plainWeightedTerm struct {
	weight    float32
	sensitive bool
	spans     [][2]int
}

func (w *plainWeightedTerm) checkPosition(pos int) bool {
	for _, sp := range w.spans {
		if pos >= sp[0] && pos <= sp[1] {
			return true
		}
	}
	return false
}

// plainWeightedTerms extracts the weighted terms of the query for one value
// (WeightedSpanTermExtractor): multi-term queries expand to the matching
// terms of the value, phrases are evaluated on its tokens.
func plainWeightedTerms(qt *hlQueryTerms, toks []hlToken) map[string]*plainWeightedTerm {
	w := map[string]*plainWeightedTerm{}
	for _, t := range qt.terms {
		w[t.text] = &plainWeightedTerm{weight: float32(t.boost)}
	}
	for _, a := range qt.automata {
		for _, tok := range toks {
			if a.match(tok.term) {
				w[tok.term] = &plainWeightedTerm{weight: float32(a.boost)}
			}
		}
	}
	for i := range qt.spans {
		sp := &qt.spans[i]
		_, ranges := hlSpanMatches(sp, toks)
		if len(ranges) == 0 {
			continue
		}
		var terms []string
		seen := map[string]bool{}
		for _, c := range sp.clauses {
			if c.prefix == nil {
				for _, term := range c.terms {
					if !seen[term] {
						seen[term] = true
						terms = append(terms, term)
					}
				}
				continue
			}
			for _, tok := range toks {
				if c.matches(tok.term) && !seen[tok.term] {
					seen[tok.term] = true
					terms = append(terms, tok.term)
				}
			}
		}
		for _, term := range terms {
			if existing := w[term]; existing != nil {
				existing.spans = append(existing.spans, ranges...)
				continue
			}
			w[term] = &plainWeightedTerm{weight: float32(sp.boost), sensitive: true, spans: append([][2]int(nil), ranges...)}
		}
	}
	return w
}

type plainFragment struct {
	text  string
	score float32
	num   int
}

// plainFragments is Highlighter.getBestTextFragments for one value: the
// fragments with a positive score, best first.
func plainFragments(value []uint16, toks []hlToken, weights map[string]*plainWeightedTerm, o *hlFieldOptions, maxFragments int) []plainFragment {
	encode := func(from, to int) string { return o.encode(string(utf16.Decode(value[from:to]))) }
	var out strings.Builder
	type frag struct {
		start, end int
		score      float32
	}
	frags := []*frag{{}}
	cur := frags[0]
	found := map[string]bool{}
	var total float32
	lastEnd := 0
	// token group
	groupTokens, groupStart, groupEnd := 0, 0, 0
	var groupTotal float32
	// fragmenters
	currentNumFrags, fragPosition, waitForPos := 1, -1, -1
	half := int(uint32(int32(o.fragmentSize)) >> 1)
	isNewFragment := func(i int) bool {
		tok := toks[i]
		switch {
		case o.numberOfFragments == 0:
			return false
		case o.fragmenter == "simple":
			isNew := tok.end >= o.fragmentSize*currentNumFrags
			if isNew {
				currentNumFrags++
			}
			return isNew
		}
		prev := -1
		if i > 0 {
			prev = toks[i-1].pos
		}
		fragPosition += tok.pos - prev
		if waitForPos <= fragPosition {
			waitForPos = -1
		} else if waitForPos != -1 {
			return false
		}
		if wt := weights[tok.term]; wt != nil {
			for _, sp := range wt.spans {
				if sp[0] == fragPosition {
					waitForPos = sp[1] + 1
					break
				}
			}
		}
		isNew := tok.end >= o.fragmentSize*currentNumFrags && len(value)-tok.end >= half
		if isNew {
			currentNumFrags++
		}
		return isNew
	}
	flush := func() {
		text := encode(groupStart, groupEnd)
		if groupTotal > 0 {
			text = o.preTag(0) + text + o.postTag(0)
		}
		if groupStart > lastEnd {
			out.WriteString(encode(lastEnd, groupStart))
		}
		out.WriteString(text)
		lastEnd = max(groupEnd, lastEnd)
	}
	for i, tok := range toks {
		if groupTokens > 0 && tok.start >= groupEnd {
			flush()
			groupTokens, groupTotal = 0, 0
			if isNewFragment(i) {
				cur.score = total
				cur.end = out.Len()
				cur = &frag{start: out.Len()}
				frags = append(frags, cur)
				found = map[string]bool{}
				total = 0
			}
		}
		var score float32
		if wt := weights[tok.term]; wt != nil && (!wt.sensitive || wt.checkPosition(tok.pos)) {
			score = wt.weight
			if !found[tok.term] {
				total += score
				found[tok.term] = true
			}
		}
		if groupTokens < 50 {
			if groupTokens == 0 {
				groupStart, groupEnd = tok.start, tok.end
				groupTotal += score
			} else {
				groupStart = min(groupStart, tok.start)
				groupEnd = max(groupEnd, tok.end)
				if score > 0 {
					groupTotal += score
				}
			}
			groupTokens++
		}
	}
	cur.score = total
	if groupTokens > 0 {
		flush()
	}
	if lastEnd < len(value) {
		out.WriteString(encode(lastEnd, len(value)))
	}
	cur.end = out.Len()
	text := out.String()
	var ranked []plainFragment
	for i, f := range frags {
		ranked = append(ranked, plainFragment{text: text[f.start:f.end], score: f.score, num: i})
	}
	// FragmentQueue: best score first, earlier fragments first on ties (a
	// stable sort orders exactly like the insertion sort it replaces)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > maxFragments {
		ranked = ranked[:maxFragments]
	}
	var positive []plainFragment
	for _, f := range ranked {
		if f.score > 0 {
			positive = append(positive, f)
		}
	}
	return positive
}

// plainHighlight highlights one field of a hit with the plain highlighter.
func plainHighlight(h *hit, t *hlTarget, all *hlQueryTerms) ([]string, error) {
	o := &t.opts
	switch o.fragmenter {
	case "", "simple", "span":
	default:
		if o.numberOfFragments != 0 {
			return nil, errSearchPhase(errIllegalArgument("unknown fragmenter option [%s] for the field [%s]", o.fragmenter, t.full))
		}
	}
	if o.maxAnalyzerOffset != nil && *o.maxAnalyzerOffset <= 0 {
		return nil, errSearchPhase(errIllegalArgument("the value [%d] of max_analyzer_offset is invalid", *o.maxAnalyzerOffset))
	}
	qt := all.forField(h.ix, t)
	values := hlFieldValues(h, t)
	numberOfFragments := o.numberOfFragments
	if numberOfFragments == 0 {
		numberOfFragments = 1
	}
	var frags []plainFragment
	for _, v := range values {
		if numberOfFragments < 0 {
			return nil, errSearchPhase(errIllegalArgument("maxSize must be >= 0 and < 2147483631; got: %d", numberOfFragments))
		}
		value := utf16.Encode([]rune(v))
		toks := hlAnalyze(h.ix, t.field, []string{v}, []int{0}, o.maxAnalyzerOffset)
		frags = append(frags, plainFragments(value, toks, plainWeightedTerms(qt, toks), o, numberOfFragments)...)
	}
	if o.scoreOrdered {
		// CollectionUtil.introSort with Math.round(o2.getScore() - o1.getScore())
		for i := 1; i < len(frags); i++ {
			for j := i; j > 0 && javaRound(float64(frags[j].score-frags[j-1].score)) > 0; j-- {
				frags[j], frags[j-1] = frags[j-1], frags[j]
			}
		}
	}
	if !(o.numberOfFragments == 0 && len(values) > 1 && len(frags) > 0) && len(frags) > numberOfFragments {
		frags = frags[:numberOfFragments]
	}
	if len(frags) > 0 {
		out := make([]string, len(frags))
		for i, f := range frags {
			out[i] = f.text
		}
		return out, nil
	}
	if o.noMatchSize > 0 && len(values) > 0 {
		value := utf16.Encode([]rune(values[0]))
		end := -1
		for _, tok := range hlAnalyze(h.ix, t.field, values[:1], []int{0}, o.maxAnalyzerOffset) {
			if tok.end >= o.noMatchSize {
				if tok.end == o.noMatchSize {
					end = o.noMatchSize
				}
				break
			}
			end = tok.end
		}
		if end > 0 {
			return []string{string(utf16.Decode(value[:end]))}, nil
		}
	}
	return nil, nil
}
