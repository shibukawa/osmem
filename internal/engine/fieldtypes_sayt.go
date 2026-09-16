package engine

import (
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/document"
	index "github.com/blevesearch/bleve_index_api"
)

// search_as_you_type subfields --------------------------------------------------
//
// Besides its root field a search_as_you_type field indexes <field>._<n>gram
// for n from 2 to max_shingle_size, the shingles of n tokens, and
// <field>._index_prefix, the edge n-grams (1 to 20 characters) of the
// max_shingle_size shingles with the end of the text padded by empty tokens
// (SearchAsYouTypeFieldMapper). Queries on a shingle field analyze their text
// into shingles of its size; the prefix field searches shingles of
// max_shingle_size.

// saytPrefixMaxChars is the longest edge n-gram of the prefix field.
const saytPrefixMaxChars = 20

// saytMaxShingleSize is the max_shingle_size of a search_as_you_type field.
func saytMaxShingleSize(f *Field) int {
	return getInt(f.Extra, "max_shingle_size", 3)
}

// saytSubfield returns the field type of a subfield of a search_as_you_type
// field, or nil when name is not one of its subfields.
func saytSubfield(parent *Field, name string) *Field {
	max := saytMaxShingleSize(parent)
	sub := *parent
	sub.Properties, sub.Fields, sub.CopyTo = nil, nil, nil
	switch {
	case name == "_index_prefix":
		sub.shingles, sub.saytPrefix = max, true
	case strings.HasPrefix(name, "_") && strings.HasSuffix(name, "gram"):
		n, err := strconv.Atoi(name[1 : len(name)-len("gram")])
		if err != nil || n < 2 || n > max || name != "_"+strconv.Itoa(n)+"gram" {
			return nil
		}
		sub.shingles = n
	default:
		return nil
	}
	return &sub
}

// saytSubfieldNames lists the subfields of a search_as_you_type field.
func saytSubfieldNames(f *Field) []string {
	names := []string{"_index_prefix"}
	for n := 2; n <= saytMaxShingleSize(f); n++ {
		names = append(names, "_"+strconv.Itoa(n)+"gram")
	}
	return names
}

// typeName is the type OpenSearch reports for a field (field_caps and
// fielddata errors).
func (f *Field) typeName() string {
	if f.saytPrefix {
		return "prefix"
	}
	return f.Type
}

// saytSearchAnalyzer wraps the search analyzer of a subfield with shingles.
func saytSearchAnalyzer(an analysis.Analyzer, name string, f *Field) (analysis.Analyzer, string) {
	return &shingleAnalyzer{base: an, size: f.shingles}, name + "/shingles" + strconv.Itoa(f.shingles)
}

// addSaytSubfields indexes the shingle and prefix subfields of one value of a
// search_as_you_type field.
func (b *docBuilder) addSaytSubfields(name string, arrayPos []uint64, text string, opts index.FieldIndexingOptions, an analysis.Analyzer, f *Field) {
	max := saytMaxShingleSize(f)
	for n := 2; n <= max; n++ {
		b.doc.AddField(document.NewTextFieldCustom(name+"._"+strconv.Itoa(n)+"gram", arrayPos, []byte(text), opts, &shingleAnalyzer{base: an, size: n}))
	}
	b.doc.AddField(document.NewTextFieldCustom(name+"._index_prefix", arrayPos, []byte(text), opts, &shingleAnalyzer{base: an, size: max, prefixes: true}))
}

// shingleAnalyzer wraps an analyzer with Lucene's FixedShingleFilter (token
// separator " ", filler token "" for position gaps). With prefixes it first
// pads the end of the text with size-1 positions (TrailingShingleTokenFilter)
// and emits the edge n-grams of the shingles, keeping longer shingles whole.
type shingleAnalyzer struct {
	base     analysis.Analyzer
	size     int
	prefixes bool
}

func (a *shingleAnalyzer) Analyze(input []byte) analysis.TokenStream {
	base := a.base.Analyze(input)
	if len(base) == 0 {
		return nil
	}
	byPos := map[int][]*analysis.Token{}
	var starts []int
	last := 0
	for _, t := range base {
		if _, seen := byPos[t.Position]; !seen {
			starts = append(starts, t.Position)
		}
		byPos[t.Position] = append(byPos[t.Position], t)
		if t.Position > last {
			last = t.Position
		}
	}
	end := last
	if a.prefixes {
		end += a.size - 1
	}
	type path struct {
		terms      []string
		start, off int
	}
	var out analysis.TokenStream
	for _, start := range starts {
		if start+a.size-1 > end {
			continue
		}
		// every path through the stacked tokens of the covered positions
		paths := []path{{}}
		for p := start; p < start+a.size; p++ {
			toks := byPos[p]
			if len(toks) == 0 {
				for i := range paths {
					paths[i].terms = append(paths[i].terms, "")
				}
				continue
			}
			next := make([]path, 0, len(paths)*len(toks))
			for _, pa := range paths {
				for _, t := range toks {
					np := path{terms: append(append([]string(nil), pa.terms...), string(t.Term)), start: pa.start, off: t.End}
					if p == start {
						np.start = t.Start
					}
					next = append(next, np)
				}
			}
			paths = next
		}
		for _, pa := range paths {
			term := strings.Join(pa.terms, " ")
			if !a.prefixes {
				out = append(out, &analysis.Token{Term: []byte(term), Start: pa.start, End: pa.off, Position: start, Type: analysis.Shingle})
				continue
			}
			runes := []rune(term)
			for n := 1; n <= len(runes) && n <= saytPrefixMaxChars; n++ {
				out = append(out, &analysis.Token{Term: []byte(string(runes[:n])), Start: pa.start, End: pa.off, Position: start, Type: analysis.Shingle})
			}
			if len(runes) > saytPrefixMaxChars {
				out = append(out, &analysis.Token{Term: []byte(term), Start: pa.start, End: pa.off, Position: start, Type: analysis.Shingle})
			}
		}
	}
	return out
}
