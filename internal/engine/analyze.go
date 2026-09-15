package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Analyze runs the _analyze API. expr is the index of /{index}/_analyze, p
// the URL parameters and raw the request body (body is its decoded form).
func (c *Cluster) Analyze(expr string, body M, p Params, raw []byte) (Response, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		src := p.Get("source")
		if src == "" {
			return fail(&Error{Status: 400, Type: "parse_exception", Reason: "request body or source parameter is required"})
		}
		raw = []byte(src)
		m, err := decodeObject(raw)
		if err != nil {
			return fail(err)
		}
		body = m
	}
	req, err := parseAnalyzeRequest(body, raw)
	if err != nil {
		return fail(err)
	}
	index := expr
	if index == "" {
		index = p.Get("index")
	}
	if err := req.validate(index); err != nil {
		return fail(err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ctx := newAnContext(nil, false)
	var ix *Index
	if index != "" {
		if ix, err = c.analyzeIndex(index); err != nil {
			return fail(err)
		}
		ctx = newAnContext(ix.Settings, true)
	}
	a, err := ctx.requestAnalyzer(req, ix)
	if err != nil {
		return fail(analysisError(err))
	}
	if !req.explain {
		count := 0
		l := newTokenList(false, nil, 0, &count, ctx.maxTokenCount)
		for _, text := range req.text {
			if err := l.add(a.analyze(text), a.posGap, a.offsetGap); err != nil {
				return fail(err)
			}
		}
		return ok(M{"tokens": l.tokens})
	}
	include := map[string]bool{}
	for _, attr := range req.attributes {
		include[strings.ToLower(attr)] = true
	}
	if !a.custom {
		count := 0
		l := newTokenList(true, include, a.attrs(len(a.filters)), &count, ctx.maxTokenCount)
		for _, text := range req.text {
			if err := l.add(a.analyze(text), a.posGap, a.offsetGap); err != nil {
				return fail(err)
			}
		}
		return ok(M{"detail": M{"custom_analyzer": false, "analyzer": M{"name": a.name, "tokens": l.tokens}}})
	}
	counts := make([]int, len(a.filters)+1)
	tokLists := make([]*tokenList, len(a.filters)+1)
	for i := range tokLists {
		tokLists[i] = newTokenList(true, include, a.attrs(i), &counts[i], ctx.maxTokenCount)
	}
	filtered := make([][]any, len(a.charFilters))
	for _, text := range req.text {
		texts := make([]string, len(a.charFilters))
		for i := range tokLists {
			var out []string
			if i == 0 {
				out = texts
			}
			if err := tokLists[i].add(a.run(text, i, out), a.posGap, a.offsetGap); err != nil {
				return fail(err)
			}
		}
		for i, t := range texts {
			filtered[i] = append(filtered[i], t)
		}
	}
	charFilters := []any{}
	for i, cf := range a.charFilters {
		charFilters = append(charFilters, M{"name": cf.name, "filtered_text": filtered[i]})
	}
	tokenFilters := []any{}
	for i, f := range a.filters {
		tokenFilters = append(tokenFilters, M{"name": f.name, "tokens": tokLists[i+1].tokens})
	}
	return ok(M{"detail": M{"custom_analyzer": true, "charfilters": charFilters,
		"tokenizer": M{"name": a.tokenizer.name, "tokens": tokLists[0].tokens}, "tokenfilters": tokenFilters}})
}

// analysisError turns component errors into OpenSearch errors.
func analysisError(err error) error {
	var u *unsupportedComponent
	if errors.As(err, &u) {
		return errUnsupported(u.what)
	}
	var c createTimeError
	if errors.As(err, &c) {
		return c.err
	}
	return err
}

// analyzeIndex resolves the single index of an _analyze request.
func (c *Cluster) analyzeIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		return ix, nil
	}
	switch ts := c.aliasTargets(name); len(ts) {
	case 0:
	case 1:
		return ts[0].ix, nil
	default:
		names := make([]string, len(ts))
		for i, t := range ts {
			names[i] = t.ix.Name
		}
		return nil, errIllegalArgument("alias [%s] has more than one index associated with it [%s], can't execute a single index op", name, strings.Join(names, ", "))
	}
	return nil, &Error{Status: 404, Type: "index_not_found_exception", Reason: "no such index [" + name + "]", Index: name,
		Extra: map[string]any{"resource.type": "index_expression", "resource.id": name}}
}

type analyzeRequest struct {
	text        []string
	analyzer    *string
	tokenizer   any
	charFilters []any
	filters     []any
	field       *string
	normalizer  *string
	explain     bool
	attributes  []string
}

// jsonField is a top-level member of a JSON object with the locations of its
// name and value (1-based line and byte column, as Jackson reports them).
type jsonField struct {
	name                string
	keyLine, keyCol     int
	valueLine, valueCol int
}

// scanTopLevelFields lists the members of a JSON object in document order.
func scanTopLevelFields(raw []byte) []jsonField {
	var out []jsonField
	line, lineStart := 1, 0
	i := 0
	skipSpace := func() {
		for i < len(raw) {
			switch raw[i] {
			case '\n':
				line++
				lineStart = i + 1
			case '\r':
				if i+1 < len(raw) && raw[i+1] == '\n' {
					i++
				}
				line++
				lineStart = i + 1
			case ' ', '\t':
			default:
				return
			}
			i++
		}
	}
	skipString := func() string {
		start := i + 1
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] == '\\' {
				i++
			}
			i++
		}
		s := string(raw[start:min(i, len(raw))])
		i++
		return s
	}
	var skipValue func()
	skipValue = func() {
		skipSpace()
		if i >= len(raw) {
			return
		}
		switch raw[i] {
		case '"':
			skipString()
		case '{', '[':
			open := raw[i]
			closeCh := byte('}')
			if open == '[' {
				closeCh = ']'
			}
			i++
			for {
				skipSpace()
				if i >= len(raw) {
					return
				}
				if raw[i] == closeCh {
					i++
					return
				}
				if raw[i] == ',' || raw[i] == ':' {
					i++
					continue
				}
				skipValue()
			}
		default:
			for i < len(raw) && !strings.ContainsRune(",}] \t\r\n", rune(raw[i])) {
				i++
			}
		}
	}
	skipSpace()
	if i >= len(raw) || raw[i] != '{' {
		return nil
	}
	i++
	for {
		skipSpace()
		if i >= len(raw) || raw[i] == '}' {
			return out
		}
		if raw[i] == ',' {
			i++
			continue
		}
		if raw[i] != '"' {
			return out
		}
		f := jsonField{keyLine: line, keyCol: i - lineStart + 1}
		f.name = skipString()
		skipSpace()
		if i < len(raw) && raw[i] == ':' {
			i++
		}
		skipSpace()
		f.valueLine, f.valueCol = line, i-lineStart+1
		skipValue()
		out = append(out, f)
	}
}

func xContentParseError(line, col int, msg string, cause *Error) *Error {
	return &Error{Status: 400, Type: "x_content_parse_exception", Reason: fmt.Sprintf("[%d:%d] [analyze_request] %s", line, col, msg), Cause: cause}
}

func xcontentTokenName(v any) string {
	switch v.(type) {
	case string:
		return "VALUE_STRING"
	case bool:
		return "VALUE_BOOLEAN"
	case nil:
		return "VALUE_NULL"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	}
	return "VALUE_NUMBER"
}

// parseAnalyzeRequest is AnalyzeAction.Request.fromXContent.
func parseAnalyzeRequest(body M, raw []byte) (*analyzeRequest, error) {
	fields := scanTopLevelFields(raw)
	if len(fields) == 0 && len(body) > 0 {
		names := make([]string, 0, len(body))
		for k := range body {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fields = append(fields, jsonField{name: k, keyLine: 1, keyCol: 1, valueLine: 1, valueCol: 1})
		}
	}
	req := &analyzeRequest{}
	for _, f := range fields {
		v, present := body[f.name]
		if !present {
			continue
		}
		unsupportedType := func() error {
			return xContentParseError(f.valueLine, f.valueCol, fmt.Sprintf("%s doesn't support values of type: %s", f.name, xcontentTokenName(v)), nil)
		}
		switch f.name {
		case "text", "attributes":
			var list []string
			switch x := v.(type) {
			case string:
				list = []string{x}
			case []any:
				list = []string{}
				for _, e := range x {
					switch e.(type) {
					case M, []any:
						return nil, xContentParseError(f.valueLine, f.valueCol, "failed to parse field ["+f.name+"]", nil)
					}
					list = append(list, analysisSettingString(e))
					if e == nil {
						list[len(list)-1] = "null"
					}
				}
			default:
				return nil, unsupportedType()
			}
			if f.name == "text" {
				req.text = list
			} else {
				req.attributes = list
			}
		case "analyzer", "field", "normalizer":
			s, ok := v.(string)
			if !ok {
				return nil, unsupportedType()
			}
			switch f.name {
			case "analyzer":
				req.analyzer = &s
			case "field":
				req.field = &s
			default:
				req.normalizer = &s
			}
		case "tokenizer":
			switch v.(type) {
			case string, M:
				req.tokenizer = v
			default:
				return nil, unsupportedType()
			}
		case "filter", "char_filter":
			var items []any
			switch x := v.(type) {
			case M:
				items = []any{x}
			case []any:
				for _, e := range x {
					switch e.(type) {
					case string, M:
						items = append(items, e)
					default:
						return nil, xContentParseError(f.valueLine, f.valueCol, "failed to parse field ["+f.name+"]",
							&Error{Type: "x_content_parse_exception", Reason: fmt.Sprintf("[%d:%d] Expected [VALUE_STRING] or [START_OBJECT], got %s", f.valueLine, f.valueCol, xcontentTokenName(e))})
					}
				}
			default:
				return nil, unsupportedType()
			}
			if f.name == "filter" {
				req.filters = append(req.filters, items...)
			} else {
				req.charFilters = append(req.charFilters, items...)
			}
		case "explain":
			switch x := v.(type) {
			case bool:
				req.explain = x
			case string:
				switch x {
				case "true":
					req.explain = true
				case "false":
					req.explain = false
				default:
					return nil, xContentParseError(f.valueLine, f.valueCol, "failed to parse field [explain]",
						&Error{Type: "illegal_argument_exception", Reason: "Failed to parse value [" + x + "] as only [true] or [false] are allowed."})
				}
			default:
				return nil, unsupportedType()
			}
		default:
			return nil, xContentParseError(f.keyLine, f.keyCol, "unknown field ["+f.name+"]", nil)
		}
	}
	return req, nil
}

// validate is AnalyzeAction.Request.validate.
func (r *analyzeRequest) validate(index string) error {
	var msgs []string
	if len(r.text) == 0 {
		msgs = append(msgs, "text is missing")
	}
	if index == "" && r.normalizer != nil {
		msgs = append(msgs, "index is required if normalizer is specified")
	}
	if r.normalizer != nil && (r.tokenizer != nil || r.analyzer != nil) {
		msgs = append(msgs, "tokenizer/analyze should be null if normalizer is specified")
	}
	extra := r.tokenizer != nil || len(r.charFilters) > 0 || len(r.filters) > 0
	if r.analyzer != nil && extra {
		msgs = append(msgs, "cannot define extra components on a named analyzer")
	}
	if r.normalizer != nil && extra {
		msgs = append(msgs, "cannot define extra components on a named normalizer")
	}
	if r.field != nil && extra {
		msgs = append(msgs, "cannot define extra components on a field-specific analyzer")
	}
	if len(msgs) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("Validation Failed: ")
	for i, m := range msgs {
		fmt.Fprintf(&sb, "%d: %s;", i+1, m)
	}
	return &Error{Status: 400, Type: "action_request_validation_exception", Reason: sb.String()}
}

// requestAnalyzer picks the analyzer of a request the way
// TransportAnalyzeAction does.
func (ctx *anContext) requestAnalyzer(r *analyzeRequest, ix *Index) (*anAnalyzer, error) {
	switch {
	case r.tokenizer != nil || len(r.filters) > 0 || len(r.charFilters) > 0:
		a, err := ctx.customChain(r.tokenizer, r.charFilters, r.filters)
		if err == nil {
			foldKuromoji(a)
		}
		return a, err
	case r.analyzer != nil:
		return ctx.analyzerByName(*r.analyzer)
	case r.normalizer != nil:
		return ctx.normalizerByName(*r.normalizer)
	case r.field != nil:
		if ix == nil {
			return nil, errAnalysis("analysis based on a specific field requires an index")
		}
		return ctx.fieldAnalyzer(ix, *r.field)
	case ix == nil:
		a, _, err := ctx.builtinAnalyzer("standard", "standard", M{})
		return a, err
	}
	return ctx.indexAnalyzer("default")
}
