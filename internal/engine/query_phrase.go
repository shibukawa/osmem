package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/searcher"
	index "github.com/blevesearch/bleve_index_api"
)

// Positional matching (phrases, spans, intervals) on Lucene positions.
//
// Bleve numbers the tokens of every value of a field from 1 and tells the
// values of a multi-valued field apart by their array positions. Lucene
// numbers the tokens of all values of a field in one sequence, leaving
// position_increment_gap positions between values. positionMapper
// converts bleve term locations to Lucene positions.

type positionMapper struct {
	ix     *Index
	reader index.IndexReader
	field  string
	f      *Field
	gap    int
	starts map[string]map[string]int // document id -> array positions -> first position
}

func newPositionMapper(ix *Index, reader index.IndexReader, field string, f *Field) *positionMapper {
	gap := 100
	if f != nil {
		gap = getInt(f.Extra, "position_increment_gap", 100)
	}
	return &positionMapper{ix: ix, reader: reader, field: field, f: f, gap: gap, starts: map[string]map[string]int{}}
}

func arrayKey(ap []uint64) string {
	var sb strings.Builder
	for i, p := range ap {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatUint(p, 10))
	}
	return sb.String()
}

// position returns the Lucene position of a term location of a document.
func (pm *positionMapper) position(id index.IndexInternalID, loc *search.Location) int {
	if len(loc.ArrayPositions) == 0 {
		return int(loc.Pos) - 1
	}
	ext, err := pm.reader.ExternalID(id)
	if err != nil {
		return int(loc.Pos) - 1
	}
	starts, ok := pm.starts[ext]
	if !ok {
		starts = pm.valueStarts(ext)
		pm.starts[ext] = starts
	}
	return starts[arrayKey(loc.ArrayPositions)] + int(loc.Pos) - 1
}

// valueStarts computes the first Lucene position of every value of the
// field in a document.
func (pm *positionMapper) valueStarts(ext string) map[string]int {
	out := map[string]int{}
	d := pm.ix.docByExternalID(ext)
	if d == nil || pm.f == nil {
		return out
	}
	_, base, ok := pm.ix.Mapping.resolve(pm.field)
	if !ok {
		return out
	}
	var vals []valPos
	leafPositions(d.Src, strings.Split(base, "."), nil, &vals)
	var an analysis.Analyzer
	if pm.f.Type == TypeText || pm.f.Type == TypeMatchOnlyText || pm.f.Type == TypeSearchAsYouType {
		an, _ = pm.ix.analysis.analyzerNamed(pm.f.Analyzer)
	}
	next := 0
	for _, v := range vals {
		out[arrayKey(v.pos)] = next
		n := 1
		if an != nil {
			n = tokenPositionCount(an, v.value)
		}
		next += n + pm.gap
	}
	return out
}

// tokenPositionCount is the number of positions a value takes: the tokens
// of the tokenizer, including those later removed by filters (Lucene keeps
// their position increments).
func tokenPositionCount(an analysis.Analyzer, text string) int {
	if da, ok := an.(*analysis.DefaultAnalyzer); ok && da.Tokenizer != nil {
		input := []byte(text)
		for _, cf := range da.CharFilters {
			input = cf.Filter(input)
		}
		return len(da.Tokenizer.Tokenize(input))
	}
	max := 0
	for _, t := range an.Analyze([]byte(text)) {
		if t.Position > max {
			max = t.Position
		}
	}
	return max
}

// docByExternalID resolves a bleve document id (root or nested object).
func (ix *Index) docByExternalID(ext string) *Doc {
	root, chain := parseNestedID(ext)
	d := ix.docs[root]
	if d == nil || chain == nil {
		return d
	}
	return ix.nestedDocByChain(d, chain)
}

// phrases ----------------------------------------------------------------

// phraseSlot is one position of a phrase: its offset from the first
// position and the terms accepted there.
type phraseSlot struct {
	offset int
	terms  []string
}

// phraseQuery is Lucene's (multi) phrase query with slop; with prefix the
// terms of the last slot are prefixes expanded to at most maxExpansions
// index terms (MultiPhrasePrefixQuery).
type phraseQuery struct {
	ix            *Index
	field         string
	f             *Field
	slots         []phraseSlot
	slop          int
	prefix        bool
	maxExpansions int
	// constant scores every match 1 (the boolean similarity)
	constant bool
}

func (q *phraseQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	slots := q.slots
	if q.prefix && len(slots) > 0 {
		last := slots[len(slots)-1]
		seen := map[string]bool{}
		var expanded []string
		max := q.maxExpansions
	expand:
		for _, p := range last.terms {
			terms, err := dictTerms(i, q.field, p)
			if err != nil {
				return nil, err
			}
			for _, t := range terms {
				if !seen[t] {
					seen[t] = true
					expanded = append(expanded, t)
				}
				if len(expanded) >= max {
					break expand
				}
			}
		}
		if len(expanded) == 0 {
			return searcher.NewMatchNoneSearcher(i)
		}
		slots = append(append([]phraseSlot(nil), slots[:len(slots)-1]...), phraseSlot{offset: last.offset, terms: expanded})
	}
	options.IncludeTermVectors = true
	var children []search.Searcher
	var owners []int // slot of each child
	for si, s := range slots {
		for _, t := range s.terms {
			ts, err := searcher.NewTermSearcher(ctx, i, t, q.field, 1, options)
			if err != nil {
				for _, c := range children {
					_ = c.Close()
				}
				return nil, err
			}
			children = append(children, ts)
			owners = append(owners, si)
		}
	}
	pm := newPositionMapper(q.ix, i, q.field, q.f)
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		out := matchPhrase(pm, q.field, slots, owners, res, q.slop)
		if q.constant {
			for i := range out {
				out[i].score = 1
			}
		}
		return out, nil
	}), nil
}

func matchPhrase(pm *positionMapper, field string, slots []phraseSlot, owners []int, res [][]docEntry, slop int) []docEntry {
	type docAcc struct {
		id      index.IndexInternalID
		score   float64
		present []bool
		pos     [][]int
		ftls    []search.FieldTermLocation
	}
	byID := map[string]*docAcc{}
	for ci, list := range res {
		si := owners[ci]
		for _, e := range list {
			key := string(e.id)
			a := byID[key]
			if a == nil {
				a = &docAcc{id: e.id, present: make([]bool, len(slots)), pos: make([][]int, len(slots))}
				byID[key] = a
			}
			a.present[si] = true
			a.score += e.score
			for li := range e.ftls {
				l := &e.ftls[li]
				if l.Field != field {
					continue
				}
				a.pos[si] = append(a.pos[si], pm.position(e.id, &l.Location))
				a.ftls = append(a.ftls, *l)
			}
		}
	}
	// terms shared between slots cannot share a document position
	conflicts := make([][]bool, len(slots))
	for a := range slots {
		conflicts[a] = make([]bool, len(slots))
		for b := range slots {
			if a != b && sharesTerm(slots[a].terms, slots[b].terms) {
				conflicts[a][b] = true
			}
		}
	}
	offsets := make([]int, len(slots))
	for k, s := range slots {
		offsets[k] = s.offset
	}
	var out []docEntry
	for _, a := range byID {
		ok := true
		for _, p := range a.present {
			if !p {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for k := range a.pos {
			sort.Ints(a.pos[k])
		}
		if !phrasePositionsMatch(a.pos, offsets, slop, conflicts) {
			continue
		}
		out = append(out, docEntry{id: a.id, score: a.score, ftls: a.ftls})
	}
	sort.Slice(out, func(x, y int) bool { return bytes.Compare(out[x].id, out[y].id) < 0 })
	return out
}

func sharesTerm(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// phrasePositionsMatch reports whether one position per slot can be picked
// so that the spread of position-offset values is at most slop, with slots
// sharing a term at different positions.
func phrasePositionsMatch(pos [][]int, offsets []int, slop int, conflicts [][]bool) bool {
	n := len(pos)
	if n == 0 {
		return false
	}
	if slop == 0 {
		for _, p := range pos[0] {
			base := p - offsets[0]
			all := true
			for k := 1; k < n; k++ {
				if !containsInt(pos[k], base+offsets[k]) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}
	chosen := make([]int, n)
	var dfs func(k, lo, hi int) bool
	dfs = func(k, lo, hi int) bool {
		if k == n {
			return true
		}
		for _, p := range pos[k] {
			v := p - offsets[k]
			nlo, nhi := lo, hi
			if k == 0 || v < nlo {
				nlo = v
			}
			if k == 0 || v > nhi {
				nhi = v
			}
			if nhi-nlo > slop {
				continue
			}
			clash := false
			for j := 0; j < k; j++ {
				if conflicts[k][j] && chosen[j] == p {
					clash = true
					break
				}
			}
			if clash {
				continue
			}
			chosen[k] = p
			if dfs(k+1, nlo, nhi) {
				return true
			}
		}
		return false
	}
	return dfs(0, 0, 0)
}

func containsInt(sorted []int, v int) bool {
	i := sort.SearchInts(sorted, v)
	return i < len(sorted) && sorted[i] == v
}

// valPos is a leaf value of a document with its position among the
// values of the field (array indices along the path).
type valPos struct {
	value string
	pos   []uint64
}

// leafPositions collects the leaf values under a dotted path with their
// array positions.
func leafPositions(v any, parts []string, pos []uint64, out *[]valPos) {
	if len(parts) == 0 {
		vals := flattenValues(v)
		for i, e := range vals {
			p := pos
			if len(vals) > 1 || len(pos) > 0 {
				p = append(append([]uint64(nil), pos...), uint64(i))
			}
			s, ok := e.(string)
			if !ok {
				s = fmt.Sprint(e)
			}
			*out = append(*out, valPos{value: s, pos: p})
		}
		return
	}
	switch t := v.(type) {
	case M:
		if next, ok := t[parts[0]]; ok {
			leafPositions(next, parts[1:], pos, out)
		}
	case []any:
		for i, e := range t {
			leafPositions(e, parts, append(append([]uint64(nil), pos...), uint64(i)), out)
		}
	}
}
