package engine

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// queryStringQuery implements query_string and simple_query_string with a
// small Lucene-syntax parser: AND/OR/NOT, +/- prefixes, parentheses,
// "phrases", field:value, wildcards, fuzzy~ and range [a TO b] / >x.
func (qb *queryBuilder) queryStringQuery(body any, simple bool) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		if s, ok := body.(string); ok {
			bm = M{"query": s}
		} else {
			return nil, errParsing("[query_string] query malformed")
		}
	}
	text := getString(bm, "query")
	fields := getStrings(bm, "fields")
	if len(fields) == 0 {
		if df := getString(bm, "default_field"); df != "" {
			fields = []string{df}
		} else {
			fields = []string{"*"}
		}
	}
	p := &qsParser{
		qb:         qb,
		toks:       tokenizeQueryString(text, simple),
		defaultAnd: strings.EqualFold(getString(bm, "default_operator"), "and"),
		fields:     fields,
		analyzer:   getString(bm, "analyzer"),
		simple:     simple,
	}
	q, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if q == nil {
		return bleve.NewMatchNoneQuery(), nil
	}
	if p.pos < len(p.toks) && !simple {
		return nil, errParsing("Failed to parse query [%s]", text)
	}
	return setBoost(q, getFloat(bm, "boost", 1)), nil
}

type qsToken struct {
	kind string // "word", "phrase", "(", ")", "AND", "OR", "NOT", "+", "-", "field"
	text string
}

func tokenizeQueryString(s string, simple bool) []qsToken {
	var toks []qsToken
	rs := []rune(s)
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(' || r == ')':
			toks = append(toks, qsToken{kind: string(r)})
			i++
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				j++
			}
			toks = append(toks, qsToken{kind: "phrase", text: string(rs[i+1 : min(j, len(rs))])})
			i = j + 1
		case (r == '+' || r == '-') && (i == 0 || unicode.IsSpace(rs[i-1]) || rs[i-1] == '(') && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]):
			toks = append(toks, qsToken{kind: string(r)})
			i++
		case simple && r == '|':
			toks = append(toks, qsToken{kind: "OR"})
			i++
		case !simple && (r == '[' || r == '{'):
			// range [a TO b]
			j := i
			for j < len(rs) && rs[j] != ']' && rs[j] != '}' {
				j++
			}
			toks = append(toks, qsToken{kind: "word", text: string(rs[i:min(j+1, len(rs))])})
			i = j + 1
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '(' && rs[j] != ')' && rs[j] != '"' && !(simple && rs[j] == '|') {
				if rs[j] == '\\' && j+1 < len(rs) {
					j += 2
					continue
				}
				j++
			}
			word := string(rs[i:j])
			i = j
			if !simple {
				switch word {
				case "AND", "&&":
					toks = append(toks, qsToken{kind: "AND"})
					continue
				case "OR", "||":
					toks = append(toks, qsToken{kind: "OR"})
					continue
				case "NOT", "!":
					toks = append(toks, qsToken{kind: "NOT"})
					continue
				}
				// field:value; value may be a phrase or a parenthesized group
				if idx := strings.Index(word, ":"); idx > 0 && !strings.HasPrefix(word, "http") {
					field := word[:idx]
					rest := word[idx+1:]
					toks = append(toks, qsToken{kind: "field", text: field})
					if rest == "" {
						continue
					}
					if rest[0] == '[' || rest[0] == '{' {
						// range: extend to the closing bracket (may contain spaces)
						start := i - len([]rune(rest))
						k := start
						for k < len(rs) && rs[k] != ']' && rs[k] != '}' {
							k++
						}
						rest = string(rs[start:min(k+1, len(rs))])
						i = k + 1
					}
					word = rest
				}
			}
			toks = append(toks, qsToken{kind: "word", text: word})
		}
	}
	return toks
}

type qsParser struct {
	qb         *queryBuilder
	toks       []qsToken
	pos        int
	defaultAnd bool
	fields     []string
	analyzer   string
	simple     bool
}

func (p *qsParser) peek() *qsToken {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *qsParser) parseOr() (query.Query, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	var parts []query.Query
	if left != nil {
		parts = append(parts, left)
	}
	for t := p.peek(); t != nil && t.kind == "OR"; t = p.peek() {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		if right != nil {
			parts = append(parts, right)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return bleve.NewDisjunctionQuery(parts...), nil
}

type qsClause struct {
	q    query.Query
	must bool
	not  bool
}

func (p *qsParser) parseAnd() (query.Query, error) {
	var clauses []qsClause
	explicitAnd := false
	for {
		t := p.peek()
		if t == nil || t.kind == ")" || t.kind == "OR" {
			break
		}
		if t.kind == "AND" {
			p.pos++
			explicitAnd = true
			continue
		}
		c, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if c.q != nil {
			clauses = append(clauses, c)
		}
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	if len(clauses) == 1 && !clauses[0].not {
		return clauses[0].q, nil
	}
	bq := bleve.NewBooleanQuery()
	hasMust, hasShould := false, false
	for _, c := range clauses {
		switch {
		case c.not:
			bq.AddMustNot(c.q)
		case c.must || explicitAnd || p.defaultAnd:
			bq.AddMust(c.q)
			hasMust = true
		default:
			bq.AddShould(c.q)
			hasShould = true
		}
	}
	if hasShould && !hasMust {
		bq.SetMinShould(1)
	}
	return bq, nil
}

func (p *qsParser) parseUnary() (qsClause, error) {
	t := p.peek()
	c := qsClause{}
	switch t.kind {
	case "+":
		p.pos++
		c.must = true
	case "-", "NOT":
		p.pos++
		c.not = true
	}
	q, err := p.parsePrimary()
	if err != nil {
		return c, err
	}
	c.q = q
	return c, nil
}

func (p *qsParser) parsePrimary() (query.Query, error) {
	t := p.peek()
	if t == nil {
		return nil, nil
	}
	fields := p.fields
	if t.kind == "field" {
		p.pos++
		fields = []string{t.text}
		t = p.peek()
		if t == nil {
			return nil, errParsing("Failed to parse query: missing value after field")
		}
		if fields[0] == "_exists_" && t.kind == "word" {
			p.pos++
			return p.qb.existsQuery(t.text), nil
		}
	}
	switch t.kind {
	case "(":
		p.pos++
		saved := p.fields
		p.fields = fields
		q, err := p.parseOr()
		p.fields = saved
		if err != nil {
			return nil, err
		}
		if nt := p.peek(); nt != nil && nt.kind == ")" {
			p.pos++
		}
		return q, nil
	case "phrase":
		p.pos++
		return p.fieldQuery(fields, func(field string) (query.Query, error) {
			return p.qb.phraseOnField(field, t.text, M{"analyzer": p.analyzer}, false)
		})
	case "word":
		p.pos++
		return p.wordQuery(fields, t.text)
	case ")":
		return nil, nil
	}
	p.pos++
	return nil, nil
}

// fieldQuery builds a query over the target fields (expanding "*").
func (p *qsParser) fieldQuery(fields []string, build func(field string) (query.Query, error)) (query.Query, error) {
	var subs []query.Query
	for _, fspec := range fields {
		name, boost := fspec, 1.0
		if idx := strings.LastIndex(fspec, "^"); idx > 0 {
			name = fspec[:idx]
			boost, _ = strconv.ParseFloat(fspec[idx+1:], 64)
		}
		var names []string
		if strings.ContainsAny(name, "*?") {
			for _, lf := range p.qb.ix.Mapping.leafFields(name) {
				f, _, _ := p.qb.ix.Mapping.resolve(lf)
				if f != nil && f.Type != TypeObject && f.Type != TypeNested && f.Type != TypeGeoPoint && f.Type != TypeBinary {
					names = append(names, lf)
				}
			}
		} else {
			names = []string{name}
		}
		for _, n := range names {
			q, err := build(n)
			if err != nil {
				if e, ok := err.(*Error); ok && e.Type == "query_shard_exception" {
					continue // lenient across fields of different types
				}
				return nil, err
			}
			if _, none := q.(*query.MatchNoneQuery); none {
				continue
			}
			subs = append(subs, setBoost(q, boost))
		}
	}
	switch len(subs) {
	case 0:
		return bleve.NewMatchNoneQuery(), nil
	case 1:
		return subs[0], nil
	}
	return bleve.NewDisjunctionQuery(subs...), nil
}

func (p *qsParser) wordQuery(fields []string, word string) (query.Query, error) {
	// ranges
	if !p.simple {
		if (strings.HasPrefix(word, "[") || strings.HasPrefix(word, "{")) && (strings.HasSuffix(word, "]") || strings.HasSuffix(word, "}")) {
			inner := word[1 : len(word)-1]
			parts := strings.SplitN(inner, " TO ", 2)
			if len(parts) == 2 {
				spec := M{}
				lo, hi := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
				if lo != "*" {
					if word[0] == '[' {
						spec["gte"] = lo
					} else {
						spec["gt"] = lo
					}
				}
				if hi != "*" {
					if word[len(word)-1] == ']' {
						spec["lte"] = hi
					} else {
						spec["lt"] = hi
					}
				}
				return p.fieldQuery(fields, func(field string) (query.Query, error) {
					return p.qb.rangeQuery(M{field: spec})
				})
			}
		}
		for _, op := range []string{">=", "<=", ">", "<"} {
			if strings.HasPrefix(word, op) {
				key := map[string]string{">=": "gte", "<=": "lte", ">": "gt", "<": "lt"}[op]
				val := word[len(op):]
				return p.fieldQuery(fields, func(field string) (query.Query, error) {
					return p.qb.rangeQuery(M{field: M{key: val}})
				})
			}
		}
	}
	// exists
	if word == "*" {
		if len(fields) == 1 && !strings.ContainsAny(fields[0], "*?") {
			return p.qb.existsQuery(fields[0]), nil
		}
		return bleve.NewMatchAllQuery(), nil
	}
	// fuzzy
	if idx := strings.LastIndex(word, "~"); idx > 0 {
		term := word[:idx]
		fuzz := word[idx+1:]
		if fuzz == "" {
			fuzz = "AUTO"
		}
		return p.fieldQuery(fields, func(field string) (query.Query, error) {
			return p.qb.fuzzyQuery(M{field: M{"value": term, "fuzziness": fuzz}})
		})
	}
	// boost suffix
	boost := 1.0
	if idx := strings.LastIndex(word, "^"); idx > 0 {
		if b, err := strconv.ParseFloat(word[idx+1:], 64); err == nil {
			boost = b
			word = word[:idx]
		}
	}
	word = strings.ReplaceAll(word, `\`, "")
	if strings.ContainsAny(word, "*?") {
		q, err := p.fieldQuery(fields, func(field string) (query.Query, error) {
			f, _, ok := p.qb.ix.Mapping.resolve(field)
			if !ok {
				return bleve.NewMatchNoneQuery(), nil
			}
			val := word
			if f.Type == TypeText {
				val = strings.ToLower(val)
			}
			if strings.HasSuffix(val, "*") && !strings.ContainsAny(strings.TrimSuffix(val, "*"), "*?") {
				return p.qb.prefixQuery(M{field: strings.TrimSuffix(val, "*")})
			}
			return p.qb.wildcardQuery(M{field: val})
		})
		if err != nil {
			return nil, err
		}
		return setBoost(q, boost), nil
	}
	q, err := p.fieldQuery(fields, func(field string) (query.Query, error) {
		return p.qb.matchOnField(field, word, M{"analyzer": p.analyzer}, matchOpts{})
	})
	if err != nil {
		return nil, err
	}
	return setBoost(q, boost), nil
}
