package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Highlighting follows OpenSearch's HighlightBuilder (request parsing and
// option merging), HighlightPhase (which fields are highlighted with which
// query) and the unified, plain and fvh highlighters (highlight_unified.go,
// highlight_plain.go, highlight_fvh.go).

// highlight request parsing ------------------------------------------------

var highlightCommonKeys = []string{
	"pre_tags", "post_tags", "order", "highlight_filter", "fragment_size", "number_of_fragments",
	"require_field_match", "boundary_scanner", "boundary_max_scan", "boundary_chars", "boundary_scanner_locale",
	"type", "fragmenter", "no_match_size", "force_source", "phrase_limit", "max_analyzer_offset", "options",
	"highlight_query",
}

var (
	highlightTopKeys   = append(append([]string(nil), highlightCommonKeys...), "tags_schema", "encoder", "fields")
	highlightFieldKeys = append(append([]string(nil), highlightCommonKeys...), "fragment_offset", "matched_fields")
)

var highlightStyledPreTags = []string{
	`<em class="hlt1">`, `<em class="hlt2">`, `<em class="hlt3">`, `<em class="hlt4">`, `<em class="hlt5">`,
	`<em class="hlt6">`, `<em class="hlt7">`, `<em class="hlt8">`, `<em class="hlt9">`, `<em class="hlt10">`,
}

// highlightOptions are the options of the highlight object or of one field;
// nil means unset. Integer options set to -1 count as unset, as in
// OpenSearch.
type highlightOptions struct {
	preTags, postTags []string
	preSet, postSet   bool
	scoreOrdered      *bool
	fragmentSize      *int
	numberOfFragments *int
	requireFieldMatch *bool
	boundaryScanner   string
	boundaryMaxScan   *int
	boundaryChars     *string
	highlighterType   *string
	fragmenter        *string
	noMatchSize       *int
	phraseLimit       *int
	maxAnalyzerOffset *int
	forceSource       *bool
	highlightQuery    any
	fragmentOffset    *int
	matchedFields     []string
}

type highlightFieldSpec struct {
	name string
	opts highlightOptions
}

// highlightSpec is a parsed highlight object.
type highlightSpec struct {
	global  highlightOptions
	encoder string
	fields  []highlightFieldSpec
}

func hlParseError(reason string, cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: reason, Cause: cause}
}

// hlFailedToParse is ObjectParser's "failed to parse field" for field of m,
// located where the parser stopped inside the field.
func hlFailedToParse(parser string, m M, field string, cause *Error) *Error {
	return hlParseError("["+parser+"] failed to parse field ["+field+"]", cause).atCause(valueEndTok(m, field))
}

func hlUnsupportedValue(parser string, m M, field string) *Error {
	return hlParseError(fmt.Sprintf("[%s] %s doesn't support values of type: %s", parser, field, jsonTokenName(m[field])), nil).at(valueTok(m, field))
}

// hlUnknownField is ObjectParser's unknown field error with its "did you
// mean" suggestions (Levenshtein similarity above 0.5).
func hlUnknownField(parser string, m M, field string, candidates []string) *Error {
	msg := fmt.Sprintf("[%s] unknown field [%s]", parser, field)
	type scored struct {
		sim  float32
		name string
	}
	var list []scored
	for _, c := range candidates {
		if sim := levenshteinSimilarity(field, c); sim > 0.5 {
			list = append(list, scored{sim, c})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].sim != list[j].sim {
			return list[i].sim > list[j].sim
		}
		return list[i].name < list[j].name
	})
	switch len(list) {
	case 0:
	case 1:
		msg += " did you mean [" + list[0].name + "]?"
	default:
		names := make([]string, len(list))
		for i, s := range list {
			names[i] = s.name
		}
		msg += " did you mean any of [" + strings.Join(names, ", ") + "]?"
	}
	return hlParseError(msg, nil).at(keyTok(m, field)).atParser(valueTok(m, field))
}

func hlString(parser string, m M, field string) (string, *Error) {
	v := m[field]
	s, ok := v.(string)
	if !ok {
		return "", hlUnsupportedValue(parser, m, field)
	}
	return s, nil
}

// hlInt parses an integer option: numbers are truncated and strings go
// through Double.parseDouble.
func hlInt(parser string, m M, field string) (int, *Error) {
	v := m[field]
	var f float64
	switch t := v.(type) {
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return 0, hlUnsupportedValue(parser, m, field)
		}
		f = n
	case float64:
		f = t
	case string:
		n, err := strconv.ParseFloat(strings.TrimFunc(t, func(r rune) bool { return r <= ' ' }), 64)
		if err != nil {
			return 0, hlFailedToParse(parser, m, field, &Error{Type: "number_format_exception", Reason: fmt.Sprintf("For input string: \"%s\"", t)})
		}
		f = n
	default:
		return 0, hlUnsupportedValue(parser, m, field)
	}
	if math.IsNaN(f) {
		return 0, nil
	}
	if f < math.MinInt32 || f > math.MaxInt32 {
		return 0, hlFailedToParse(parser, m, field, &Error{Type: "illegal_argument_exception", Reason: fmt.Sprintf("Value [%v] is out of range for an integer", v)})
	}
	return int(f), nil
}

func hlBool(parser string, m M, field string) (bool, *Error) {
	v := m[field]
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, hlFailedToParse(parser, m, field, &Error{Type: "illegal_argument_exception", Reason: "Failed to parse value [" + t + "] as only [true] or [false] are allowed."})
	}
	return false, hlUnsupportedValue(parser, m, field)
}

func hlStrings(parser string, m M, field string) ([]string, *Error) {
	v := m[field]
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for i, e := range t {
			switch s := e.(type) {
			case string:
				out = append(out, s)
			case json.Number:
				out = append(out, s.String())
			case float64:
				out = append(out, strconv.FormatFloat(s, 'f', -1, 64))
			case bool:
				out = append(out, strconv.FormatBool(s))
			default:
				return nil, hlFailedToParse(parser, m, field, noTextAt(e, elemTok(t, i)))
			}
		}
		return out, nil
	}
	return nil, hlUnsupportedValue(parser, m, field)
}

func hlIntPtr(n int) *int { return &n }

// parseOption parses an option shared by the highlight object and its
// fields; handled is false for unknown keys.
func (o *highlightOptions) parseOption(parser string, m M, k string) (handled bool, err *Error) {
	v := m[k]
	intOpt := func(dst **int) {
		var n int
		if n, err = hlInt(parser, m, k); err == nil {
			*dst = hlIntPtr(n)
		}
	}
	boolOpt := func(dst **bool) {
		var b bool
		if b, err = hlBool(parser, m, k); err == nil {
			*dst = &b
		}
	}
	strOpt := func(dst **string) {
		var s string
		if s, err = hlString(parser, m, k); err == nil {
			*dst = &s
		}
	}
	switch k {
	case "pre_tags":
		if o.preTags, err = hlStrings(parser, m, k); err == nil {
			o.preSet = true
		}
	case "post_tags":
		if o.postTags, err = hlStrings(parser, m, k); err == nil {
			o.postSet = true
		}
	case "order":
		var s string
		if s, err = hlString(parser, m, k); err == nil {
			score := strings.EqualFold(s, "score")
			o.scoreOrdered = &score
		}
	case "highlight_filter", "force_source":
		var b *bool
		boolOpt(&b)
		if k == "force_source" {
			o.forceSource = b
		}
	case "fragment_size":
		intOpt(&o.fragmentSize)
	case "number_of_fragments":
		intOpt(&o.numberOfFragments)
	case "require_field_match":
		boolOpt(&o.requireFieldMatch)
	case "boundary_scanner":
		var s string
		if s, err = hlString(parser, m, k); err == nil {
			switch up := strings.ToUpper(s); up {
			case "CHARS", "WORD", "SENTENCE":
				o.boundaryScanner = strings.ToLower(up)
			default:
				err = hlFailedToParse(parser, m, k, &Error{Type: "illegal_argument_exception",
					Reason: "No enum constant org.opensearch.search.fetch.subphase.highlight.HighlightBuilder.BoundaryScannerType." + up})
			}
		}
	case "boundary_max_scan":
		intOpt(&o.boundaryMaxScan)
	case "boundary_chars":
		strOpt(&o.boundaryChars)
	case "boundary_scanner_locale":
		var s *string
		strOpt(&s)
	case "type":
		strOpt(&o.highlighterType)
	case "fragmenter":
		strOpt(&o.fragmenter)
	case "no_match_size":
		intOpt(&o.noMatchSize)
	case "phrase_limit":
		intOpt(&o.phraseLimit)
	case "max_analyzer_offset":
		intOpt(&o.maxAnalyzerOffset)
	case "options":
		if _, ok := v.(M); !ok {
			err = hlUnsupportedValue(parser, m, k)
		}
	case "highlight_query":
		if _, ok := v.(M); !ok {
			err = hlUnsupportedValue(parser, m, k)
		} else {
			o.highlightQuery = v
		}
	default:
		return false, nil
	}
	return true, err
}

// parseHighlight parses and validates a highlight object. The document
// order of keys is not available, so tags_schema is applied after
// pre_tags and post_tags.
func parseHighlight(parent M, key string) (*highlightSpec, error) {
	v := parent[key]
	m, ok := v.(M)
	if !ok {
		return nil, errParsing("Unknown key for a %s in [highlight].", jsonTokenName(v)).at(valueTok(parent, key))
	}
	spec := &highlightSpec{}
	var schema any
	for _, k := range sortedMapKeys(m) {
		val := m[k]
		switch k {
		case "tags_schema":
			schema = val
		case "encoder":
			s, err := hlString("highlight", m, k)
			if err != nil {
				return nil, err
			}
			spec.encoder = s
		case "fields":
			fields, err := parseHighlightFields(m, k)
			if err != nil {
				return nil, err
			}
			spec.fields = fields
		default:
			handled, err := spec.global.parseOption("highlight", m, k)
			if err != nil {
				return nil, err
			}
			if !handled {
				return nil, hlUnknownField("highlight", m, k, highlightTopKeys)
			}
		}
	}
	if schema != nil {
		s, err := hlString("highlight", m, "tags_schema")
		if err != nil {
			return nil, err
		}
		switch s {
		case "default":
			spec.global.preTags, spec.global.postTags = []string{"<em>"}, []string{"</em>"}
		case "styled":
			spec.global.preTags, spec.global.postTags = highlightStyledPreTags, []string{"</em>"}
		default:
			return nil, hlFailedToParse("highlight", m, "tags_schema", &Error{Type: "illegal_argument_exception", Reason: "Unknown tag schema [" + s + "]"})
		}
		spec.global.preSet, spec.global.postSet = true, true
	}
	if spec.global.preSet && !spec.global.postSet {
		return nil, errParsing("pre_tags are set but post_tags are not set").at(endTok(m))
	}
	return spec, nil
}

func parseHighlightFields(parent M, key string) ([]highlightFieldSpec, error) {
	v := parent[key]
	fieldsErr := func(cause *Error) error { return hlFailedToParse("highlight", parent, key, cause) }
	var fields []highlightFieldSpec
	parseOne := func(holder M, name string) error {
		body := holder[name]
		fm, ok := body.(M)
		if !ok {
			return fieldsErr(hlFailedToParse("fields", holder, name,
				hlParseError("[highlight_field] Expected START_OBJECT but was: "+jsonTokenName(body), nil).at(valueTok(holder, name))))
		}
		f := highlightFieldSpec{name: name}
		for _, k := range sortedMapKeys(fm) {
			var err *Error
			switch k {
			case "fragment_offset":
				var n int
				if n, err = hlInt("highlight_field", fm, k); err == nil {
					f.opts.fragmentOffset = hlIntPtr(n)
				}
			case "matched_fields":
				f.opts.matchedFields, err = hlStrings("highlight_field", fm, k)
			default:
				var handled bool
				handled, err = f.opts.parseOption("highlight_field", fm, k)
				if err == nil && !handled {
					err = hlUnknownField("highlight_field", fm, k, highlightFieldKeys)
				}
			}
			if err != nil {
				return fieldsErr(hlFailedToParse("fields", holder, name, err))
			}
		}
		if f.opts.preSet && !f.opts.postSet {
			return fieldsErr(hlFailedToParse("fields", holder, name, errParsing("pre_tags are set but post_tags are not set").at(endTok(fm))))
		}
		fields = append(fields, f)
		return nil
	}
	switch t := v.(type) {
	case M:
		for _, name := range sortedMapKeys(t) {
			if err := parseOne(t, name); err != nil {
				return nil, err
			}
		}
	case []any:
		for _, e := range t {
			em, ok := e.(M)
			if !ok || len(em) != 1 {
				// reported at the second field of an entry
				at := noTok
				if ok && len(em) > 1 {
					at = nthKeyTok(em, 2)
				}
				return nil, fieldsErr(hlParseError("[fields] can be a single object with any number of fields or an array where each entry is an object with a single field", nil).at(at))
			}
			for name := range em {
				if err := parseOne(em, name); err != nil {
					return nil, err
				}
			}
		}
	default:
		return nil, hlUnsupportedValue("highlight", parent, key)
	}
	return fields, nil
}

// effective options ----------------------------------------------------------

// hlFieldOptions are the options a field is highlighted with, after merging
// the field options, the highlight object and the defaults.
type hlFieldOptions struct {
	preTags, postTags []string
	scoreOrdered      bool
	fragmentSize      int
	numberOfFragments int
	requireFieldMatch bool
	boundaryScanner   string
	boundaryMaxScan   int
	boundaryChars     string
	highlighterType   string
	fragmenter        string
	noMatchSize       int
	phraseLimit       int
	maxAnalyzerOffset *int
	forceSource       bool
	highlightQuery    any
	fragmentOffset    int
	matchedFields     []string
	encoder           string
}

func (spec *highlightSpec) options(f *highlightOptions) hlFieldOptions {
	g := &spec.global
	pickInt := func(fv, gv *int, def int) int {
		if fv != nil && *fv != -1 {
			return *fv
		}
		if gv != nil && *gv != -1 {
			return *gv
		}
		return def
	}
	pickBool := func(fv, gv *bool, def bool) bool {
		if fv != nil {
			return *fv
		}
		if gv != nil {
			return *gv
		}
		return def
	}
	pickStr := func(fv, gv *string, def string) string {
		if fv != nil {
			return *fv
		}
		if gv != nil {
			return *gv
		}
		return def
	}
	o := hlFieldOptions{
		preTags:           []string{"<em>"},
		postTags:          []string{"</em>"},
		scoreOrdered:      pickBool(f.scoreOrdered, g.scoreOrdered, false),
		fragmentSize:      pickInt(f.fragmentSize, g.fragmentSize, 100),
		numberOfFragments: pickInt(f.numberOfFragments, g.numberOfFragments, 5),
		requireFieldMatch: pickBool(f.requireFieldMatch, g.requireFieldMatch, true),
		boundaryScanner:   f.boundaryScanner,
		boundaryMaxScan:   pickInt(f.boundaryMaxScan, g.boundaryMaxScan, 20),
		boundaryChars:     pickStr(f.boundaryChars, g.boundaryChars, ".,!? \t\n"),
		highlighterType:   pickStr(f.highlighterType, g.highlighterType, "unified"),
		fragmenter:        pickStr(f.fragmenter, g.fragmenter, ""),
		noMatchSize:       pickInt(f.noMatchSize, g.noMatchSize, 0),
		phraseLimit:       pickInt(f.phraseLimit, g.phraseLimit, 256),
		forceSource:       pickBool(f.forceSource, g.forceSource, false),
		highlightQuery:    f.highlightQuery,
		fragmentOffset:    -1,
		matchedFields:     f.matchedFields,
		encoder:           spec.encoder,
	}
	if o.boundaryScanner == "" {
		o.boundaryScanner = g.boundaryScanner
	}
	if f.preSet {
		o.preTags = f.preTags
	} else if g.preSet {
		o.preTags = g.preTags
	}
	if f.postSet {
		o.postTags = f.postTags
	} else if g.postSet {
		o.postTags = g.postTags
	}
	if f.maxAnalyzerOffset != nil {
		o.maxAnalyzerOffset = f.maxAnalyzerOffset
	} else {
		o.maxAnalyzerOffset = g.maxAnalyzerOffset
	}
	if o.highlightQuery == nil {
		o.highlightQuery = g.highlightQuery
	}
	if f.fragmentOffset != nil {
		o.fragmentOffset = *f.fragmentOffset
	}
	if o.encoder == "" {
		o.encoder = "default"
	}
	return o
}

func (o *hlFieldOptions) preTag(i int) string {
	if len(o.preTags) == 0 {
		return ""
	}
	return o.preTags[i%len(o.preTags)]
}

func (o *hlFieldOptions) postTag(i int) string {
	if len(o.postTags) == 0 {
		return ""
	}
	return o.postTags[i%len(o.postTags)]
}

// htmlEncode is Lucene's SimpleHTMLEncoder.
func htmlEncode(s string) string {
	if !strings.ContainsAny(s, "\"&<>'/") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("&quot;")
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\'':
			b.WriteString("&#x27;")
		case '/':
			b.WriteString("&#x2F;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (o *hlFieldOptions) encode(s string) string {
	if o.encoder == "html" {
		return htmlEncode(s)
	}
	return s
}
