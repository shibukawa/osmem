package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/search/searcher"
	index "github.com/blevesearch/bleve_index_api"
)

// more_like_this: extracts significant terms from the "like" documents/text
// (respecting min_term_freq, stop_words, word length), drops any term also
// significant in "unlike", then keeps the terms whose document frequency in
// the target index falls in [min_doc_freq, max_doc_freq], weighted by a
// tf*idf-like score and capped to max_query_terms per field. Document
// frequency needs the shard's IndexReader, so that filtering and the final
// term weighting happen in moreLikeThisQuery.Searcher, not while the query
// is parsed.

type mltDocRef struct {
	index string
	id    string
	hasID bool
	doc   M
}

type moreLikeThisSpec struct {
	fields         []string
	likeText       []string
	likeDocs       []mltDocRef
	unlikeText     []string
	unlikeDocs     []mltDocRef
	minTermFreq    int
	maxQueryTerms  int
	minDocFreq     int
	maxDocFreq     int // -1: unbounded
	minWordLen     int
	maxWordLen     int
	stopWords      map[string]bool
	include        bool
	minShouldMatch string
}

func parseMoreLikeThis(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &moreLikeThisSpec{minTermFreq: 2, maxQueryTerms: 25, minDocFreq: 5, maxDocFreq: -1, minShouldMatch: "30%"}
	hasLike := false
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "fields":
			for _, e := range asArray(v) {
				spec.fields = append(spec.fields, xText(e))
			}
		case "like":
			hasLike = true
			spec.likeText, spec.likeDocs = parseMLTLikeList(v)
		case "unlike":
			spec.unlikeText, spec.unlikeDocs = parseMLTLikeList(v)
		case "min_term_freq":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.minTermFreq = nv
		case "max_query_terms":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxQueryTerms = nv
		case "min_doc_freq":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.minDocFreq = nv
		case "max_doc_freq":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxDocFreq = nv
		case "min_word_length", "min_word_len":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.minWordLen = nv
		case "max_word_length", "max_word_len":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxWordLen = nv
		case "stop_words":
			spec.stopWords = map[string]bool{}
			for _, e := range asArray(v) {
				spec.stopWords[strings.ToLower(xText(e))] = true
			}
		case "include":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.include = b
		case "minimum_should_match":
			spec.minShouldMatch = xText(v)
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pXContent("[more_like_this] unknown field [%s]", k)
		}
	}
	if !hasLike {
		return nil, pIllegalArgument("more_like_this requires 'like' to be specified")
	}
	n.spec = spec
	return n, nil
}

func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return []any{v}
}

func parseMLTLikeList(v any) (texts []string, docs []mltDocRef) {
	for _, item := range asArray(v) {
		m, isObj := item.(M)
		if !isObj {
			texts = append(texts, xText(item))
			continue
		}
		ref := mltDocRef{index: xText(m["_index"])}
		if docBody, ok := m["doc"].(M); ok {
			ref.doc = docBody
		} else if id, ok := m["_id"]; ok {
			ref.id, ref.hasID = xText(id), true
		}
		docs = append(docs, ref)
	}
	return texts, docs
}

// mltDefaultFields is the fields "like" is matched against when "fields" is
// not given: every text leaf field of the mapping.
func mltDefaultFields(mp *Mapping) []string {
	var out []string
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for name, f := range fields {
			path := prefix + name
			switch f.Type {
			case TypeText:
				out = append(out, path)
			case TypeObject, TypeNested:
				walk(path+".", f.Properties)
			}
		}
	}
	walk("", mp.Properties)
	sort.Strings(out)
	return out
}

// termFreqMap tokenizes text (an nil: the whole text is one term, as for a
// keyword-like field) into how many times each surviving term occurs.
func termFreqMap(an analysis.Analyzer, text string, stopWords map[string]bool, minLen, maxLen int) map[string]int {
	keep := func(term string) bool {
		if stopWords[strings.ToLower(term)] {
			return false
		}
		n := utf8.RuneCountInString(term)
		return (minLen <= 0 || n >= minLen) && (maxLen <= 0 || n <= maxLen)
	}
	freq := map[string]int{}
	if an == nil {
		if keep(text) {
			freq[text]++
		}
		return freq
	}
	for _, tok := range an.Analyze([]byte(text)) {
		term := string(tok.Term)
		if keep(term) {
			freq[term]++
		}
	}
	return freq
}

func (qb *queryBuilder) mltDocSources(refs []mltDocRef) []M {
	var out []M
	for _, ref := range refs {
		if ref.doc != nil {
			out = append(out, ref.doc)
			continue
		}
		ix := qb.ix
		if ref.index != "" && ref.index != qb.ix.Name {
			other, err := qb.c.resolveWriteIndex(ref.index)
			if err != nil {
				continue
			}
			ix = other
		}
		if d := ix.docs[ref.id]; d != nil && d.Src != nil {
			out = append(out, d.Src)
		}
	}
	return out
}

func (qb *queryBuilder) moreLikeThisToQuery(spec *moreLikeThisSpec) (query.Query, error) {
	fields := spec.fields
	if len(fields) == 0 {
		fields = mltDefaultFields(qb.ix.Mapping)
	}
	likeSrcs := qb.mltDocSources(spec.likeDocs)
	unlikeSrcs := qb.mltDocSources(spec.unlikeDocs)

	likeFreq := map[string]map[string]int{}
	unlikeTerms := map[string]map[string]bool{}
	addFreq := func(field, text string, an analysis.Analyzer) {
		for term, n := range termFreqMap(an, text, spec.stopWords, spec.minWordLen, spec.maxWordLen) {
			if likeFreq[field] == nil {
				likeFreq[field] = map[string]int{}
			}
			likeFreq[field][term] += n
		}
	}
	addUnlike := func(field, text string, an analysis.Analyzer) {
		for term := range termFreqMap(an, text, spec.stopWords, spec.minWordLen, spec.maxWordLen) {
			if unlikeTerms[field] == nil {
				unlikeTerms[field] = map[string]bool{}
			}
			unlikeTerms[field][term] = true
		}
	}
	for _, field := range fields {
		f, _, mapped := qb.ix.Mapping.resolve(field)
		if !mapped {
			continue
		}
		var an analysis.Analyzer
		if f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType {
			an, _ = qb.ix.analysis.analyzerNamed(f.Analyzer)
		}
		for _, src := range likeSrcs {
			for _, val := range flattenValues(lookupPath(src, field)) {
				addFreq(field, fmt.Sprint(val), an)
			}
		}
		for _, text := range spec.likeText {
			addFreq(field, text, an)
		}
		for _, src := range unlikeSrcs {
			for _, val := range flattenValues(lookupPath(src, field)) {
				addUnlike(field, fmt.Sprint(val), an)
			}
		}
		for _, text := range spec.unlikeText {
			addUnlike(field, text, an)
		}
	}

	var candidates []mltCandidate
	for field, freq := range likeFreq {
		path := qb.ix.Mapping.searchPath(field)
		for term, tf := range freq {
			if tf < spec.minTermFreq || unlikeTerms[field][term] {
				continue
			}
			candidates = append(candidates, mltCandidate{field: path, term: term, tf: tf})
		}
	}
	if len(candidates) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}

	var exclude []string
	if !spec.include {
		for _, ref := range spec.likeDocs {
			if ref.hasID && (ref.index == "" || ref.index == qb.ix.Name) {
				exclude = append(exclude, ref.id)
			}
		}
	}
	return &moreLikeThisQuery{
		candidates: candidates, minDocFreq: spec.minDocFreq, maxDocFreq: spec.maxDocFreq,
		maxQueryTerms: spec.maxQueryTerms, minShouldMatch: spec.minShouldMatch, exclude: exclude,
	}, nil
}

type mltCandidate struct {
	field string
	term  string
	tf    int
}

// moreLikeThisQuery finishes significant-term selection once the
// IndexReader (and so document frequencies) is available.
type moreLikeThisQuery struct {
	candidates     []mltCandidate
	minDocFreq     int
	maxDocFreq     int
	maxQueryTerms  int
	minShouldMatch string
	exclude        []string
}

func (q *moreLikeThisQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	numDocs, _ := i.DocCount()
	type weighted struct {
		term   string
		weight float64
	}
	byField := map[string][]weighted{}
	for _, c := range q.candidates {
		df, err := dictDocFreq(i, c.field, c.term)
		if err != nil {
			return nil, err
		}
		if df == 0 {
			continue
		}
		if q.minDocFreq > 0 && int(df) < q.minDocFreq {
			continue
		}
		if q.maxDocFreq >= 0 && int(df) > q.maxDocFreq {
			continue
		}
		idf := math.Log(float64(numDocs+1)/float64(df+1)) + 1
		byField[c.field] = append(byField[c.field], weighted{term: c.term, weight: float64(c.tf) * idf})
	}
	var subs []query.Query
	for _, field := range sortedMapKeysOf(byField) {
		list := byField[field]
		sort.Slice(list, func(a, b int) bool {
			if list[a].weight != list[b].weight {
				return list[a].weight > list[b].weight
			}
			return list[a].term < list[b].term
		})
		if q.maxQueryTerms > 0 && len(list) > q.maxQueryTerms {
			list = list[:q.maxQueryTerms]
		}
		for _, w := range list {
			tq := bleve.NewTermQuery(w.term)
			tq.SetField(field)
			subs = append(subs, tq)
		}
	}
	if len(subs) == 0 {
		return searcher.NewMatchNoneSearcher(i)
	}
	bq := &luceneBoolQuery{should: subs, minShould: minimumShouldMatch(q.minShouldMatch, len(subs))}
	if len(q.exclude) > 0 {
		bq.mustNot = []query.Query{bleve.NewDocIDQuery(q.exclude)}
	}
	return bq.Searcher(ctx, i, m, options)
}

func sortedMapKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
