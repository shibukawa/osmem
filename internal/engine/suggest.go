package engine

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2/analysis"
)

// suggest.go implements the "suggest" section of _search (SuggestPhase):
// term, phrase and completion suggesters. Real OpenSearch scores phrase
// suggestions with a Lucene language model with internal smoothing this
// cannot reproduce exactly; osmem instead scores candidate phrases with a
// simple stupid-backoff n-gram model built from the field's stored values,
// which orders corrections reasonably without matching Lucene's float
// scores. The term and completion suggesters match OpenSearch's own
// behaviour (edit distance and prefix matching are deterministic).

// suggestSpec is a parsed "suggest" section.
type suggestSpec struct {
	items []*suggestionSpec
}

// suggestionSpec is one named suggestion of a suggest section.
type suggestionSpec struct {
	name       string
	text       string
	textSet    bool
	regex      bool // the input was given as "regex" rather than "text"/"prefix"
	term       *termSuggesterSpec
	phrase     *phraseSuggesterSpec
	completion *completionSuggesterSpec
}

// parseSuggestSpec is SuggestBuilder.fromXContent, building the structures
// runSuggest executes.
func parseSuggestSpec(r bodyReader, m M) (*suggestSpec, error) {
	spec := &suggestSpec{}
	globalText, hasGlobalText := "", false
	for _, k := range r.keys(m, "suggest") {
		v := m[k]
		switch t := v.(type) {
		case M:
			item, err := parseSuggestionSpec(k, t)
			if err != nil {
				return nil, err
			}
			spec.items = append(spec.items, item)
		case []any, nil:
			return nil, pParsing("unexpected token [%s] after [%s]", jsonTokenName(v), k).at(valueTok(m, k))
		default:
			if k != "text" {
				return nil, pIllegalArgument("[suggest] does not support [%s]", k)
			}
			globalText, hasGlobalText = xText(v), true
		}
	}
	if len(spec.items) == 0 {
		return nil, nil
	}
	for _, item := range spec.items {
		if !item.textSet && hasGlobalText {
			item.text, item.textSet = globalText, true
		}
	}
	return spec, nil
}

func parseSuggestionSpec(name string, m M) (*suggestionSpec, error) {
	item := &suggestionSpec{name: name}
	kind := ""
	for _, k := range keysSorted(m) {
		v := m[k]
		switch k {
		case "text", "prefix":
			item.text, item.textSet, item.regex = xText(v), true, false
		case "regex":
			item.text, item.textSet, item.regex = xText(v), true, true
		case "term", "phrase", "completion":
			tm, ok := v.(M)
			if !ok {
				return nil, pParsing("suggestion does not support [%s]", k).at(valueTok(m, k))
			}
			var err error
			switch k {
			case "term":
				item.term, err = parseTermSuggester(tm)
			case "phrase":
				item.phrase, err = parsePhraseSuggester(tm)
			case "completion":
				item.completion, err = parseCompletionSuggester(tm)
			}
			if err != nil {
				return nil, err
			}
			kind = k
		default:
			if _, isObj := v.(M); isObj {
				return nil, parseFailure((&Error{Status: http.StatusBadRequest, Type: "named_object_not_found_exception", Reason: "unknown field [" + k + "]"}).at(valueTok(m, k)))
			}
			return nil, pParsing("suggestion does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if kind == "" {
		return nil, parseFailure(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "missing suggestion object"})
	}
	return item, nil
}

// stringOr returns s, or def when s is empty.
func stringOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// analyzerOr returns the suggester's own analyzer override, or the field's.
func analyzerOr(explicit, fieldDefault string) string { return stringOr(explicit, fieldDefault) }

// term suggester -----------------------------------------------------------

type termSuggesterSpec struct {
	field         string
	size          int
	suggestMode   string
	sortMode      string
	analyzer      string
	maxEdits      int
	prefixLength  int
	minWordLength int
	accuracy      float64
}

// parseTermSuggester is TermSuggestionBuilder.fromXContent's field
// defaults (SuggestionSearchContext.SuggestionContext / DirectSpellChecker).
func parseTermSuggester(m M) (*termSuggesterSpec, error) {
	field := getString(m, "field")
	if field == "" {
		return nil, pParse("the required field option [%s] is missing", "field")
	}
	return &termSuggesterSpec{
		field: field, size: getInt(m, "size", 5), suggestMode: stringOr(getString(m, "suggest_mode"), "missing"),
		sortMode: stringOr(getString(m, "sort"), "score"), analyzer: getString(m, "analyzer"),
		maxEdits: getInt(m, "max_edits", 2), prefixLength: getInt(m, "prefix_length", 1),
		minWordLength: getInt(m, "min_word_length", 4), accuracy: getFloat(m, "accuracy", 0.5),
	}, nil
}

// dictEntry is one term of a field's dictionary.
type dictFreq = map[string]uint64

// fieldDictEntries lists the terms indexed for a field, with their document
// frequency (bleve's field dictionary, as highlight_fvh.go's fvhIndexTerms
// reads it).
func fieldDictEntries(ix *Index, field string) (dictFreq, error) {
	dict, err := ix.bleve.FieldDict(field)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dict.Close() }()
	out := dictFreq{}
	for {
		e, err := dict.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			return out, nil
		}
		out[e.Term] += e.Count
	}
}

// suggestIndices lists the distinct live indices of a set of targets.
func suggestIndices(ts []target, live map[*Index]bool) []*Index {
	seen := map[*Index]bool{}
	var out []*Index
	for _, t := range ts {
		if live != nil && !live[t.ix] {
			continue
		}
		if !seen[t.ix] {
			seen[t.ix] = true
			out = append(out, t.ix)
		}
	}
	return out
}

func (c *Cluster) runTermSuggestion(ts []target, live map[*Index]bool, spec *termSuggesterSpec, text string) ([]M, error) {
	idxs := suggestIndices(ts, live)
	if len(idxs) == 0 {
		return []M{{"text": text, "offset": 0, "length": utf16Length(text), "options": []any{}}}, nil
	}
	var an analysis.Analyzer
	freq := dictFreq{}
	for _, ix := range idxs {
		f, _, ok := ix.Mapping.resolve(spec.field)
		if !ok {
			e := errIllegalArgument("no mapping found for field [%s]", spec.field)
			e.Index = ix.Name
			return nil, errSearchPhase(e)
		}
		a, err := ix.analysis.analyzerNamed(analyzerOr(spec.analyzer, f.Analyzer))
		if err != nil {
			return nil, err
		}
		an = a
		m, derr := fieldDictEntries(ix, spec.field)
		if derr != nil {
			return nil, derr
		}
		for term, n := range m {
			freq[term] += n
		}
	}
	toks := an.Analyze([]byte(text))
	offsets := utf16Offsets(text)
	out := make([]M, 0, len(toks))
	for _, tok := range toks {
		word := string(tok.Term)
		out = append(out, M{
			"text": word, "offset": offsets[tok.Start], "length": offsets[tok.End] - offsets[tok.Start],
			"options": termCandidates(freq, word, spec),
		})
	}
	return out, nil
}

// termCandidates is DirectSpellChecker.suggestSimilar for one word: every
// dictionary term within max_edits (Damerau-Levenshtein, as osmem's fuzzy
// queries already compute it) sharing the word's prefix_length prefix,
// filtered by suggest_mode and accuracy, ranked by sort and truncated to
// size. Ties beyond size may survive in a different order than OpenSearch's
// internal priority queue would keep, but the ranked set matches.
func termCandidates(freq dictFreq, word string, spec *termSuggesterSpec) []any {
	wordRunes := []rune(word)
	if len(wordRunes) < spec.minWordLength {
		return []any{}
	}
	origFreq := freq[word]
	prefix := wordRunes
	if spec.prefixLength < len(wordRunes) {
		prefix = wordRunes[:spec.prefixLength]
	}
	type cand struct {
		term  string
		score float64
		freq  uint64
	}
	var cands []cand
	for term, f := range freq {
		if term == word {
			continue
		}
		termRunes := []rune(term)
		if len(prefix) > 0 {
			if len(termRunes) < len(prefix) || string(termRunes[:len(prefix)]) != string(prefix) {
				continue
			}
		}
		dist := editDistance(wordRunes, termRunes, true, spec.maxEdits)
		if dist > spec.maxEdits {
			continue
		}
		minLen := len(wordRunes)
		if len(termRunes) < minLen {
			minLen = len(termRunes)
		}
		if minLen == 0 {
			continue
		}
		score := 1 - float64(dist)/float64(minLen)
		if score < spec.accuracy {
			continue
		}
		switch spec.suggestMode {
		case "missing":
			if origFreq > 0 {
				continue
			}
		case "popular":
			if f <= origFreq {
				continue
			}
		}
		cands = append(cands, cand{term, score, f})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if spec.sortMode == "frequency" {
			if a.freq != b.freq {
				return a.freq > b.freq
			}
			if a.score != b.score {
				return a.score > b.score
			}
		} else {
			if a.score != b.score {
				return a.score > b.score
			}
			if a.freq != b.freq {
				return a.freq > b.freq
			}
		}
		return a.term < b.term
	})
	if len(cands) > spec.size {
		cands = cands[:spec.size]
	}
	out := make([]any, len(cands))
	for i, cd := range cands {
		out[i] = M{"text": cd.term, "score": Float(float32(cd.score)), "freq": int64(cd.freq)}
	}
	return out
}

// phrase suggester -----------------------------------------------------------

type directGeneratorSpec struct {
	field         string
	suggestMode   string
	size          int
	maxEdits      int
	prefixLength  int
	minWordLength int
}

type phraseHighlightSpec struct{ preTag, postTag string }

type phraseSuggesterSpec struct {
	field      string
	size       int
	gramSize   int
	maxErrors  float64
	confidence float64
	analyzer   string
	generators []directGeneratorSpec
	highlight  *phraseHighlightSpec
}

func parsePhraseSuggester(m M) (*phraseSuggesterSpec, error) {
	field := getString(m, "field")
	if field == "" {
		return nil, pParse("the required field option [%s] is missing", "field")
	}
	spec := &phraseSuggesterSpec{
		field: field, size: getInt(m, "size", 5), gramSize: getInt(m, "gram_size", 2),
		maxErrors: getFloat(m, "max_errors", 1), confidence: getFloat(m, "confidence", 1), analyzer: getString(m, "analyzer"),
	}
	if spec.gramSize <= 0 {
		spec.gramSize = 2
	}
	if gens, ok := m["direct_generator"].([]any); ok {
		for _, g := range gens {
			gm, ok := g.(M)
			if !ok {
				continue
			}
			gf := getString(gm, "field")
			if gf == "" {
				continue
			}
			spec.generators = append(spec.generators, directGeneratorSpec{
				field: gf, suggestMode: stringOr(getString(gm, "suggest_mode"), "missing"),
				size: getInt(gm, "size", 5), maxEdits: getInt(gm, "max_edits", 2),
				prefixLength: getInt(gm, "prefix_length", 1), minWordLength: getInt(gm, "min_word_length", 4),
			})
		}
	}
	if hm, ok := m["highlight"].(M); ok {
		spec.highlight = &phraseHighlightSpec{preTag: getString(hm, "pre_tag"), postTag: getString(hm, "post_tag")}
	}
	return spec, nil
}

// phraseMaxErrors resolves max_errors (an absolute count, or a fraction of
// the word count) to a number of positions.
func phraseMaxErrors(maxErrors float64, n int) int {
	if maxErrors >= 1 {
		return int(maxErrors)
	}
	return int(math.Ceil(maxErrors * float64(n)))
}

// phraseLM is a tiny stupid-backoff n-gram language model built from a
// field's stored values, used to rank candidate phrase corrections. It is a
// structural approximation of Lucene's smoothed language models (linear
// interpolation / Laplace / stupid backoff), not a reimplementation of any
// one of them: absolute scores will not match OpenSearch, but a correction
// whose n-grams occur in the corpus consistently outranks one that does not.
type phraseLM struct {
	gram    int
	total   int
	unigram map[string]int
	ngram   map[string]int
	context map[string]int
}

func newPhraseLM(gram int) *phraseLM {
	if gram < 2 {
		gram = 2
	}
	return &phraseLM{gram: gram, unigram: map[string]int{}, ngram: map[string]int{}, context: map[string]int{}}
}

func (lm *phraseLM) addText(words []string) {
	for _, w := range words {
		lm.unigram[w]++
		lm.total++
	}
	g := lm.gram
	for i := 0; i+g <= len(words); i++ {
		lm.ngram[strings.Join(words[i:i+g], "\x1f")]++
		lm.context[strings.Join(words[i:i+g-1], "\x1f")]++
	}
}

// addIndex feeds every stored value of field in ix through an into the
// model.
func (lm *phraseLM) addIndex(ix *Index, field string, an analysis.Analyzer) {
	if _, _, ok := ix.Mapping.resolve(field); !ok {
		return
	}
	for _, d := range ix.docs {
		if d.nested != nil {
			continue
		}
		for _, v := range ix.sourceLeafValues(d, field) {
			if s, ok := v.(string); ok {
				lm.addText(tokens(an, s))
			}
		}
	}
}

const stupidBackoffDiscount = 0.4

// score is a stupid-backoff score for a candidate word sequence: the
// product of each word's probability given the words before it, backed off
// to a Laplace-smoothed unigram probability where the n-gram was never
// observed (the first g-1 words, having no full context yet, always score
// on unigrams). A correction is only worth more than the original phrase
// when it is genuinely more frequent in context, not merely context-free.
func (lm *phraseLM) score(words []string) float64 {
	if len(words) == 0 {
		return 0
	}
	g := lm.gram
	unigramProb := func(w string) float64 {
		return (float64(lm.unigram[w]) + 1) / float64(lm.total+len(lm.unigram)+1)
	}
	score := 1.0
	for i := 0; i < len(words); i++ {
		if i < g-1 {
			score *= unigramProb(words[i])
			continue
		}
		start := i + 1 - g
		key := strings.Join(words[start:i+1], "\x1f")
		ctxKey := strings.Join(words[start:i], "\x1f")
		if c := lm.ngram[key]; c > 0 {
			score *= float64(c) / float64(lm.context[ctxKey])
			continue
		}
		score *= stupidBackoffDiscount * unigramProb(words[i])
	}
	return score
}

func highlightPhrase(words []string, changed []bool, hl *phraseHighlightSpec) string {
	parts := make([]string, len(words))
	for i, w := range words {
		if changed[i] {
			parts[i] = hl.preTag + w + hl.postTag
		} else {
			parts[i] = w
		}
	}
	return strings.Join(parts, " ")
}

// runPhraseSuggestion implements the phrase suggester: per-position
// candidates come only from configured direct_generator entries (as
// OpenSearch requires; without one, the only candidate is the input
// phrase itself), combined up to max_errors positions and ranked with
// phraseLM.
func (c *Cluster) runPhraseSuggestion(ts []target, live map[*Index]bool, spec *phraseSuggesterSpec, text string) ([]M, error) {
	idxs := suggestIndices(ts, live)
	if len(idxs) == 0 {
		return []M{{"text": text, "offset": 0, "length": utf16Length(text), "options": []any{}}}, nil
	}
	var an analysis.Analyzer
	for _, ix := range idxs {
		f, _, ok := ix.Mapping.resolve(spec.field)
		if !ok {
			e := errIllegalArgument("no mapping found for field [%s]", spec.field)
			e.Index = ix.Name
			return nil, errSearchPhase(e)
		}
		a, err := ix.analysis.analyzerNamed(analyzerOr(spec.analyzer, f.Analyzer))
		if err != nil {
			return nil, err
		}
		an = a
	}
	origWords := tokens(an, text)
	if len(origWords) == 0 {
		return []M{{"text": text, "offset": 0, "length": utf16Length(text), "options": []any{}}}, nil
	}

	lm := newPhraseLM(spec.gramSize)
	for _, ix := range idxs {
		lm.addIndex(ix, spec.field, an)
	}

	freqByField := map[string]dictFreq{}
	getFreq := func(field string) (dictFreq, error) {
		if m, ok := freqByField[field]; ok {
			return m, nil
		}
		m := dictFreq{}
		for _, ix := range idxs {
			if _, _, ok := ix.Mapping.resolve(field); !ok {
				continue
			}
			dm, err := fieldDictEntries(ix, field)
			if err != nil {
				return nil, err
			}
			for term, f := range dm {
				m[term] += f
			}
		}
		freqByField[field] = m
		return m, nil
	}

	type posCandidate struct {
		word    string
		changed bool
	}
	positions := make([][]posCandidate, len(origWords))
	for i, w := range origWords {
		set := map[string]bool{w: true}
		list := []posCandidate{{word: w}}
		for _, gen := range spec.generators {
			field := stringOr(gen.field, spec.field)
			freq, err := getFreq(field)
			if err != nil {
				return nil, err
			}
			ts := &termSuggesterSpec{field: field, size: gen.size, suggestMode: gen.suggestMode, sortMode: "score",
				maxEdits: gen.maxEdits, prefixLength: gen.prefixLength, minWordLength: gen.minWordLength}
			for _, o := range termCandidates(freq, w, ts) {
				cand := o.(M)["text"].(string)
				if !set[cand] {
					set[cand] = true
					list = append(list, posCandidate{word: cand, changed: true})
				}
			}
		}
		positions[i] = list
	}

	maxChanges := phraseMaxErrors(spec.maxErrors, len(origWords))
	type phraseCand struct {
		words   []string
		changed []bool
		score   float64
	}
	var results []phraseCand
	const maxCombinations = 20000
	var walk func(i, changes int, words []string, changed []bool) bool
	walk = func(i, changes int, words []string, changed []bool) bool {
		if len(results) >= maxCombinations {
			return false
		}
		if i == len(positions) {
			results = append(results, phraseCand{words: append([]string(nil), words...), changed: append([]bool(nil), changed...), score: lm.score(words)})
			return true
		}
		for _, cnd := range positions[i] {
			nc := changes
			if cnd.changed {
				nc++
				if nc > maxChanges {
					continue
				}
			}
			if !walk(i+1, nc, append(words, cnd.word), append(changed, cnd.changed)) {
				return false
			}
		}
		return true
	}
	walk(0, 0, nil, nil)

	sort.SliceStable(results, func(i, j int) bool { return results[i].score > results[j].score })
	origScore := lm.score(origWords)
	options := make([]any, 0, spec.size)
	seen := map[string]bool{}
	for _, res := range results {
		phrase := strings.Join(res.words, " ")
		if seen[phrase] {
			continue
		}
		if res.score < spec.confidence*origScore {
			continue
		}
		seen[phrase] = true
		opt := M{"text": phrase, "score": Float(float32(res.score))}
		if spec.highlight != nil {
			opt["highlighted"] = highlightPhrase(res.words, res.changed, spec.highlight)
		}
		options = append(options, opt)
		if len(options) >= spec.size {
			break
		}
	}
	return []M{{"text": text, "offset": 0, "length": utf16Length(text), "options": options}}, nil
}

// completion suggester -------------------------------------------------------

type completionFuzzySpec struct {
	fuzziness      *fuzzinessSpec
	prefixLength   int
	minLength      int
	transpositions bool
}

type completionSuggesterSpec struct {
	field          string
	size           int
	skipDuplicates bool
	fuzzy          *completionFuzzySpec
	contexts       M
}

func parseCompletionSuggester(m M) (*completionSuggesterSpec, error) {
	field := getString(m, "field")
	if field == "" {
		return nil, pParse("the required field option [%s] is missing", "field")
	}
	spec := &completionSuggesterSpec{field: field, size: getInt(m, "size", 5), skipDuplicates: getBool(m, "skip_duplicates", false)}
	if cv, has := m["contexts"]; has {
		if cm, ok := cv.(M); ok {
			spec.contexts = cm
		} else {
			spec.contexts = M{}
		}
	}
	switch fv := m["fuzzy"].(type) {
	case M:
		fz := &completionFuzzySpec{prefixLength: getInt(fv, "prefix_length", 1), minLength: getInt(fv, "min_length", 3),
			transpositions: getBool(fv, "transpositions", true)}
		fuzziness := "AUTO"
		if v, ok := fv["fuzziness"]; ok {
			fuzziness = xText(v)
		}
		fs, err := buildFuzziness(fuzziness)
		if err != nil {
			return nil, err
		}
		fz.fuzziness = fs
		spec.fuzzy = fz
	case bool:
		if fv {
			fz := &completionFuzzySpec{prefixLength: 1, minLength: 3, transpositions: true}
			fz.fuzziness, _ = buildFuzziness("AUTO")
			spec.fuzzy = fz
		}
	}
	return spec, nil
}

// parseCompletionEntry reads one stored value of a completion field
// (sourceLeafValues already lists them one by one): a bare string (weight
// 1, no contexts) or an {input, weight, contexts} object.
func parseCompletionEntry(raw any) (inputs []string, weight int64, contexts M, ok bool) {
	switch t := raw.(type) {
	case string:
		return []string{t}, 1, nil, true
	case M:
		switch iv := t["input"].(type) {
		case string:
			inputs = []string{iv}
		case []any:
			for _, e := range iv {
				if s, ok := e.(string); ok {
					inputs = append(inputs, s)
				}
			}
		default:
			return nil, 0, nil, false
		}
		weight = 1
		if wv, has := t["weight"]; has {
			weight = completionWeightValue(wv)
		}
		if cm, ok := t["contexts"].(M); ok {
			contexts = cm
		}
		return inputs, weight, contexts, len(inputs) > 0
	}
	return nil, 0, nil, false
}

func completionWeightValue(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimPrefix(t, "+"), 10, 64)
		return n
	}
	return 1
}

// completionMatches is AnalyzingSuggester/FuzzySuggester matching: the
// query, analyzed and its tokens rejoined with spaces (preserve_separators),
// must be a literal prefix of the candidate input analyzed the same way, or
// within the fuzzy edit distance of some prefix of it.
func completionMatches(query, input string, fuzzy *completionFuzzySpec) bool {
	if strings.HasPrefix(input, query) {
		return true
	}
	if fuzzy == nil {
		return false
	}
	qr := []rune(query)
	if len(qr) < fuzzy.minLength {
		return false
	}
	pl := fuzzy.prefixLength
	if pl > len(qr) {
		pl = len(qr)
	}
	ir := []rune(input)
	if pl > len(ir) || string(qr[:pl]) != string(ir[:pl]) {
		return false
	}
	limit := fuzzy.fuzziness.distance(query)
	if limit <= 0 {
		return false
	}
	return prefixEditDistance(qr, ir, fuzzy.transpositions, limit) <= limit
}

// prefixEditDistance is the minimum edit distance between q and any prefix
// of t: a full Damerau-Levenshtein table (as editDistance computes, see
// query_regexp.go) read for the smallest value on q's last row, rather than
// only its last cell.
func prefixEditDistance(q, t []rune, transpositions bool, limit int) int {
	n, m := len(q), len(t)
	if n == 0 {
		return 0
	}
	prev2 := make([]int, m+1)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if q[i-1] == t[j-1] {
				cost = 0
			}
			v := prev[j] + 1
			if cur[j-1]+1 < v {
				v = cur[j-1] + 1
			}
			if prev[j-1]+cost < v {
				v = prev[j-1] + cost
			}
			if transpositions && i > 1 && j > 1 && q[i-1] == t[j-2] && q[i-2] == t[j-1] && prev2[j-2]+1 < v {
				v = prev2[j-2] + 1
			}
			cur[j] = v
		}
		prev2, prev, cur = prev, cur, prev2
	}
	best := prev[0]
	for j := 1; j <= m; j++ {
		if prev[j] < best {
			best = prev[j]
		}
	}
	if best > limit {
		return limit + 1
	}
	return best
}

func (c *Cluster) runCompletionSuggestion(ts []target, live map[*Index]bool, spec *completionSuggesterSpec, text string) ([]M, error) {
	type option struct {
		text   string
		weight int64
		ix     *Index
		doc    *Doc
	}
	var opts []option
	for _, ix := range suggestIndices(ts, live) {
		f, _, ok := ix.Mapping.resolve(spec.field)
		if !ok {
			e := errIllegalArgument("no mapping found for field [%s]", spec.field)
			e.Index = ix.Name
			return nil, errSearchPhase(e)
		}
		if f.Type != TypeCompletion {
			e := errIllegalArgument("Field [%s] is not a completion suggest field", spec.field)
			e.Index = ix.Name
			return nil, errSearchPhase(e)
		}
		defs := completionContexts(f)
		queryCtx, cerr := parseContextQuery(defs, spec.contexts)
		if cerr != nil {
			cerr.Index = ix.Name
			return nil, errSearchPhase(cerr)
		}
		an, aerr := ix.analysis.analyzerNamed(f.Analyzer)
		if aerr != nil {
			return nil, aerr
		}
		normQuery := strings.Join(tokens(an, text), " ")
		for _, d := range ix.docs {
			if d.nested != nil {
				continue
			}
			for _, raw := range ix.sourceLeafValues(d, spec.field) {
				inputs, weight, ctxVals, ok := parseCompletionEntry(raw)
				if !ok {
					continue
				}
				if !matchesContextQuery(defs, queryCtx, ctxVals, d) {
					continue
				}
				for _, in := range inputs {
					normInput := strings.Join(tokens(an, in), " ")
					if completionMatches(normQuery, normInput, spec.fuzzy) {
						opts = append(opts, option{text: in, weight: weight, ix: ix, doc: d})
					}
				}
			}
		}
	}
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].weight > opts[j].weight })
	out := make([]any, 0, spec.size)
	seen := map[string]bool{}
	for _, o := range opts {
		if spec.skipDuplicates {
			if seen[o.text] {
				continue
			}
			seen[o.text] = true
		}
		om := M{"text": o.text, "_index": o.ix.Name, "_id": o.doc.ID, "_score": Float(float32(o.weight))}
		indexSource := mappingSourceFilter(o.ix.Mapping)
		if !indexSource.disabled {
			if indexSource.isPlain() {
				om["_source"] = json.RawMessage(o.doc.Raw)
			} else if src, ok := applySourceFilters(o.doc.Src, indexSource); ok {
				om["_source"] = src
			}
		}
		out = append(out, om)
		if len(out) >= spec.size {
			break
		}
	}
	return []M{{"text": text, "offset": 0, "length": utf16Length(text), "options": out}}, nil
}

// dispatch -------------------------------------------------------------------

// runSuggest executes the suggest section of a search request against its
// resolved (live) targets and renders the "suggest" response section.
func (c *Cluster) runSuggest(ts []target, sr *searchRequest, live map[*Index]bool) (M, error) {
	out := M{}
	for _, item := range sr.suggest.items {
		var entries []M
		var err error
		switch {
		case item.term != nil:
			entries, err = c.runTermSuggestion(ts, live, item.term, item.text)
		case item.phrase != nil:
			entries, err = c.runPhraseSuggestion(ts, live, item.phrase, item.text)
		case item.completion != nil:
			entries, err = c.runCompletionSuggestion(ts, live, item.completion, item.text)
		}
		if err != nil {
			return nil, err
		}
		list := make([]any, len(entries))
		for i, e := range entries {
			list[i] = e
		}
		out[item.name] = list
	}
	return out, nil
}
