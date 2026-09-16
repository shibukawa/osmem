package engine

import (
	"math"
	"net/http"
	"sort"
	"strings"
)

// significant_terms and significant_text --------------------------------------------
//
// Both find terms that are statistically over-represented in a foreground
// ("subset") set of documents compared to a background ("superset") set —
// by default the whole index, or background_filter's matches. A heuristic
// (default jlh) scores each candidate term; only terms where
// subsetFreq/subsetSize exceeds supersetFreq/supersetSize are kept, sorted
// by score descending then key ascending, and truncated to size (default
// 10; confirmed against a live OpenSearch 3.8.0 instance since none of this
// is documented at the level of exact arithmetic).
//
// significant_text differs only in how terms are produced: instead of
// reading indexed doc values (which requires fielddata on text fields), it
// re-analyzes the original _source text of the configured field(s) on the
// fly, so it also works on fields without fielddata/doc_values.

type sigHeuristic int

const (
	heurJLH sigHeuristic = iota
	heurChiSquare
	heurGND
	heurMutualInformation
	heurPercentage
)

type significantSpec struct {
	vs               vsConfig
	text             bool
	fields           []string // significant_text source field(s)
	dedupe           bool     // significant_text filter_duplicate_text
	size, shardSize  int
	minDoc, shardMin int64
	ie               *includeExclude
	bgFilter         any
	heuristic        sigHeuristic
	bgSuperset       bool
}

var sigTermsFields = valuesSourceFields("significant_terms", false, false, map[string]int{
	"size": vtNumber, "shard_size": vtNumber, "min_doc_count": vtNumber, "shard_min_doc_count": vtNumber,
	"background_filter": vtObject, "execution_hint": vtString,
	"include": vtObjectArrayOrString, "exclude": vtStringArray,
	"jlh": vtObject, "chi_square": vtObject, "gnd": vtObject, "mutual_information": vtObject,
	"percentage": vtObject, "script_heuristic": vtObject,
})

// parseSigHeuristic reads the (at most one) significance heuristic clause;
// jlh and percentage take no options, the others take background_is_superset.
func parseSigHeuristic(of objFields, body M) (sigHeuristic, bool, error) {
	names := []string{"jlh", "chi_square", "gnd", "mutual_information", "percentage", "script_heuristic"}
	found := ""
	for _, n := range names {
		if _, ok := body[n]; ok {
			if found != "" {
				return 0, false, of.failed(body, n, errIllegalArgument("Only one significance heuristic can be defined per significant terms aggregation"))
			}
			found = n
		}
	}
	sub, _ := body[found].(M)
	switch found {
	case "percentage":
		return heurPercentage, true, nil
	case "script_heuristic":
		return 0, false, errUnsupported("[script_heuristic] in significance heuristics")
	case "chi_square":
		return heurChiSquare, getBool(sub, "background_is_superset", true), nil
	case "gnd":
		return heurGND, getBool(sub, "background_is_superset", true), nil
	case "mutual_information":
		return heurMutualInformation, getBool(sub, "background_is_superset", true), nil
	default:
		return heurJLH, true, nil
	}
}

// parseSignificanceCommon parses the fields shared by significant_terms and
// significant_text (BucketCountThresholds, include/exclude, background
// selection and the heuristic).
func parseSignificanceCommon(of objFields, d *aggDef, spec *significantSpec) error {
	body := d.body
	if err := parseThresholds(of, d, &spec.size, &spec.shardSize, &spec.minDoc, &spec.shardMin); err != nil {
		return err
	}
	var err error
	if spec.ie, err = parseIncludeExclude(of, body); err != nil {
		return err
	}
	spec.bgFilter = body["background_filter"]
	if spec.heuristic, spec.bgSuperset, err = parseSigHeuristic(of, body); err != nil {
		return err
	}
	return nil
}

func parseSignificantTerms(ps *aggParser, d *aggDef) error {
	of := sigTermsFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &significantSpec{size: 10, shardSize: -1, minDoc: 3}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if err := parseSignificanceCommon(of, d, spec); err != nil {
		return err
	}
	if err := requireFieldOrScript(body); err != nil {
		return err
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

var sigTextFields = objFields{name: "significant_text", fields: map[string]int{
	"field": vtString, "size": vtNumber, "shard_size": vtNumber, "min_doc_count": vtNumber, "shard_min_doc_count": vtNumber,
	"background_filter": vtObject, "filter_duplicate_text": vtBool, "source_fields": vtStringArray,
	"include": vtObjectArrayOrString, "exclude": vtStringArray,
	"jlh": vtObject, "chi_square": vtObject, "gnd": vtObject, "mutual_information": vtObject,
	"percentage": vtObject, "script_heuristic": vtObject,
}}

func parseSignificantText(ps *aggParser, d *aggDef) error {
	of := sigTextFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	field, _ := body["field"].(string)
	if field == "" {
		return errIllegalArgument("Required one of fields [field, script], but none were specified. ")
	}
	spec := &significantSpec{text: true, size: 10, shardSize: -1, minDoc: 3}
	spec.fields = getStrings(body, "source_fields")
	if len(spec.fields) == 0 {
		spec.fields = []string{field}
	}
	spec.dedupe = getBool(body, "filter_duplicate_text", false)
	if err := parseSignificanceCommon(of, d, spec); err != nil {
		return err
	}
	d.spec = spec
	return nil
}

func prepareSignificantTerms(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*significantSpec)
	vs, err := pc.resolve(d, 0, &spec.vs, "significant_terms", vsBytes, termsKinds...)
	if err != nil {
		return err
	}
	if vs.kind == vsNumeric && vs.floating {
		return &Error{Status: http.StatusInternalServerError, Type: "unsupported_operation_exception", Reason: "No support for examining floating point numerics"}
	}
	// ie.compile only rejects regex include/exclude for the numeric-family
	// kinds; IP needs the same rejection (with a different message, since it
	// is bytes-like, not numeric) but is not one of those kinds
	if vs.kind == vsIP && spec.ie != nil && spec.ie.regexBased() {
		return errIllegalArgument("Aggregation [%s] cannot support regular expression style include/exclude settings as they can only be applied to string fields. Use an array of values for include/exclude clauses", d.name)
	}
	accept, err := spec.ie.compile(d, vs)
	if err != nil {
		return err
	}
	pc.ac.setAux(d, 0, pc.ix, accept)
	return nil
}

// sigTextAux is the per-index preparation of significant_text: the analyzer
// of each source field (fieldAnalyzer, the same resolution _analyze uses)
// and the compiled include/exclude filter.
type sigTextAux struct {
	ans    []*anAnalyzer
	accept func(termKey) bool
}

func prepareSignificantText(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*significantSpec)
	ctx := newAnContext(pc.ix.Settings, true)
	aux := &sigTextAux{}
	for _, f := range spec.fields {
		an, err := ctx.fieldAnalyzer(pc.ix, f)
		if err != nil {
			return err
		}
		aux.ans = append(aux.ans, an)
	}
	accept, err := spec.ie.compile(d, &valuesSource{kind: vsBytes})
	if err != nil {
		return err
	}
	aux.accept = accept
	pc.ac.setAux(d, 0, pc.ix, aux)
	return nil
}

// sigTermsTerms extracts the (filtered) indexed term values of a hit.
func sigTermsTerms(ac *aggContext, d *aggDef, h *hit) []termKey {
	vs := ac.source(d, 0, h.ix)
	if vs == nil || (vs.unmapped && vs.missing == nil) {
		return nil
	}
	vals := termValues(vs, h)
	accept, _ := ac.aux(d, 0, h.ix).(func(termKey) bool)
	if accept == nil {
		return vals
	}
	out := make([]termKey, 0, len(vals))
	for _, k := range vals {
		if accept(k) {
			out = append(out, k)
		}
	}
	return out
}

// sigTextTerms re-analyzes the _source text of a hit's source field(s) into
// its unique terms (deduplicated the way SortedSetDocValues would be).
func sigTextTerms(ac *aggContext, d *aggDef, h *hit) []termKey {
	spec := d.spec.(*significantSpec)
	aux, _ := ac.aux(d, 0, h.ix).(*sigTextAux)
	if aux == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []termKey
	for i, field := range spec.fields {
		an := aux.ans[i]
		for _, v := range h.ix.fieldValues(h.doc, field) {
			s, ok := v.(string)
			if !ok {
				continue
			}
			for _, tok := range an.analyze(s).tokens {
				if seen[tok.term] {
					continue
				}
				k := termKey{s: tok.term}
				if aux.accept != nil && !aux.accept(k) {
					continue
				}
				seen[tok.term] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// sigTextRaw joins the raw source field(s) of a hit for filter_duplicate_text.
func sigTextRaw(h *hit, fields []string) string {
	var sb strings.Builder
	for _, f := range fields {
		for _, v := range h.ix.fieldValues(h.doc, f) {
			if s, ok := v.(string); ok {
				sb.WriteString(s)
				sb.WriteByte(0)
			}
		}
	}
	return sb.String()
}

type sigDocKey struct {
	ix  *Index
	doc *Doc
}

func collectSignificant(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*significantSpec)
	background, err := ac.allHits()
	if err != nil {
		return nil, err
	}
	if spec.bgFilter != nil {
		if background, err = ac.filterHits(spec.bgFilter, background); err != nil {
			return nil, err
		}
	}
	background = docOrder(background)

	// terms caches, per document, the (deduplicated) term keys it
	// contributes; filter_duplicate_text approximates OpenSearch's
	// byte-level near-duplicate shingle filter (DuplicateByteSequenceSpotter)
	// with exact whole-text-value deduplication, in first-seen order.
	termCache := map[sigDocKey][]termKey{}
	seenText := map[string]bool{}
	terms := func(h *hit) []termKey {
		k := sigDocKey{h.ix, h.doc}
		if v, ok := termCache[k]; ok {
			return v
		}
		var out []termKey
		if spec.text && spec.dedupe {
			raw := sigTextRaw(h, spec.fields)
			if !seenText[raw] {
				seenText[raw] = true
				out = sigTextTerms(ac, d, h)
			}
		} else if spec.text {
			out = sigTextTerms(ac, d, h)
		} else {
			out = sigTermsTerms(ac, d, h)
		}
		termCache[k] = out
		return out
	}

	bg := map[termKey]int64{}
	for _, h := range background {
		for _, tk := range terms(h) {
			bg[tk]++
		}
	}
	fg := map[termKey]int64{}
	byKey := map[termKey][]*hit{}
	var order []termKey
	for _, h := range hits {
		for _, tk := range terms(h) {
			if _, ok := fg[tk]; !ok {
				order = append(order, tk)
			}
			fg[tk]++
			byKey[tk] = append(byKey[tk], h)
		}
	}

	subsetSize, supersetSize := float64(len(hits)), float64(len(background))
	var cands []*bucket
	for _, k := range order {
		subF := fg[k]
		if subF < spec.minDoc {
			continue
		}
		rawSupF := bg[k]
		supF, supSize := float64(rawSupF), supersetSize
		if !spec.bgSuperset {
			supF += float64(subF)
			supSize += subsetSize
		}
		if float64(subF)/subsetSize <= supF/supSize {
			continue // not over-represented in the foreground: not significant
		}
		score := sigScore(spec.heuristic, float64(subF), subsetSize, supF, supSize)
		cands = append(cands, &bucket{sortKey: k, docCount: subF, hits: byKey[k],
			fields: M{"score": score, "bg_count": rawSupF}})
	}

	keyCmp := termsKeyCompare(ac, d) // falls back to plain string order for significant_text
	sort.SliceStable(cands, func(i, j int) bool {
		si, sj := cands[i].fields["score"].(float64), cands[j].fields["score"].(float64)
		if si != sj {
			return si > sj
		}
		return keyCmp(cands[i], cands[j]) < 0
	})
	if len(cands) > spec.size {
		cands = cands[:spec.size]
	}
	if err := ac.collectSubs(d, cands); err != nil {
		return nil, err
	}

	typed, javaClass := "sigsterms", "SignificantStringTerms"
	if spec.text {
		for _, b := range cands {
			k := b.sortKey.(termKey)
			b.key, b.keyString = k.s, k.s
		}
	} else {
		cls, _, err := ac.termsClass(d, 0)
		if err != nil {
			return nil, err
		}
		for _, b := range cands {
			ac.renderTermKey(d, b, cls)
		}
		switch cls {
		case "lterms":
			typed, javaClass = "siglterms", "SignificantLongTerms"
		case "dterms":
			typed, javaClass = "sigdterms", "SignificantDoubleTerms"
		}
	}
	return &aggResult{kind: resBuckets, typed: typed, javaClass: javaClass, buckets: cands,
		fields: M{"doc_count": int64(len(hits)), "bg_count": int64(len(background))}}, nil
}

// sigScore dispatches to the configured significance heuristic. subF/supF
// and subSize/supSize are already adjusted for background_is_superset.
func sigScore(h sigHeuristic, subF, subSize, supF, supSize float64) float64 {
	switch h {
	case heurPercentage:
		return subF / supF
	case heurChiSquare:
		return sigChiSquare(subF, subSize, supF, supSize)
	case heurGND:
		return sigGND(subF, subSize, supF, supSize)
	case heurMutualInformation:
		return sigMutualInformation(subF, subSize, supF, supSize)
	default:
		return sigJLH(subF, subSize, supF, supSize)
	}
}

// sigJLH is JLHScore.getScore (the default heuristic): https://github.com/opensearch-project/OpenSearch/blob/3.8.0/server/src/main/java/org/opensearch/search/aggregations/bucket/terms/heuristic/JLHScore.java
// Formula and all others below verified numerically against a live
// OpenSearch 3.8.0 instance (exact scores reproduced to float64 precision).
func sigJLH(subF, subSize, supF, supSize float64) float64 {
	subProb, supProb := subF/subSize, supF/supSize
	if subProb > supProb {
		return (subProb - supProb) * (subProb / supProb)
	}
	return 0
}

// sigChiSquare is Pearson's chi-squared statistic over the term/class 2x2
// contingency table (ChiSquare.getScore).
func sigChiSquare(subF, subSize, supF, supSize float64) float64 {
	a, b := subF, subSize-subF
	c, d := supF-subF, supSize-subSize-(supF-subF)
	n := supSize
	diff := a*d - b*c
	return n * diff * diff / ((a + b) * (c + d) * (a + c) * (b + d))
}

// sigGND is GND.getScore: the Google Normalized Distance between the class
// and the term, mapped from a [0,1] dissimilarity (0 = identical) to a score
// via exp(-distance), so a larger score means more significant.
func sigGND(subF, subSize, supF, supSize float64) float64 {
	fx, fy, fxy, n := subSize, supF, subF, supSize
	num := math.Max(math.Log(fx), math.Log(fy)) - math.Log(fxy)
	den := math.Log(n) - math.Min(math.Log(fx), math.Log(fy))
	return math.Exp(-(num / den))
}

// sigMutualInformation is MutualInformation.getScore over the same 2x2
// contingency table as chi-square (Manning & Schütze feature-selection MI).
func sigMutualInformation(subF, subSize, supF, supSize float64) float64 {
	a, b := subF, subSize-subF
	c, d := supF-subF, supSize-subSize-(supF-subF)
	n := supSize
	n1, n0 := a+b, c+d
	m1, m0 := a+c, b+d
	term := func(x, row, col float64) float64 {
		if x <= 0 {
			return 0
		}
		return (x / n) * math.Log2((n*x)/(row*col))
	}
	return term(a, n1, m1) + term(b, n1, m0) + term(c, n0, m1) + term(d, n0, m0)
}
