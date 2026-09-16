package engine

import "sort"

// rare_terms ------------------------------------------------------------------------
//
// Unlike terms, real OpenSearch approximates rare_terms with a cuckoo filter
// (precision controls its false-positive rate); osmem holds the full data set
// in memory, so it computes exact doc counts instead and treats precision as
// accepted-but-unused. There is no size/shard_size/order/min_doc_count: every
// term with doc count <= max_doc_count is returned, ordered by ascending doc
// count then ascending key (RareTermsAggregatorFactory / RareTermsAggregator).

var rareTermsFields = valuesSourceFields("rare_terms", true, false, map[string]int{
	"max_doc_count": vtNumber, "precision": vtNumber,
	"include": vtObjectArrayOrString, "exclude": vtStringArray,
})

type rareTermsSpec struct {
	vs          vsConfig
	maxDocCount int64
	precision   float64
	ie          *includeExclude
}

func parseRareTerms(ps *aggParser, d *aggDef) error {
	of := rareTermsFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &rareTermsSpec{maxDocCount: 1, precision: 0.001}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["max_doc_count"]; ok {
		n, err := of.longValue(body, "max_doc_count")
		if err != nil {
			return err
		}
		if n <= 0 {
			return of.failed(body, "max_doc_count", errIllegalArgument("[max_doc_count] must be greater than 0. Found [%d] in [%s]", n, d.name))
		}
		// the real bound check and message both read this way (off-by-one
		// wording bug in RareTermsAggregationBuilder.setMaxDocCount)
		if n > 100 {
			return of.failed(body, "max_doc_count", errIllegalArgument("[max_doc_count] must be smallerthan 100in [%s]", d.name))
		}
		spec.maxDocCount = n
	}
	if _, ok := body["precision"]; ok {
		p, err := of.doubleValue(body, "precision")
		if err != nil {
			return err
		}
		if p < 0.00001 {
			return of.failed(body, "precision", errIllegalArgument("[precision] must be greater than 0.00001"))
		}
		spec.precision = p
	}
	if spec.ie, err = parseIncludeExclude(of, body); err != nil {
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

func prepareRareTerms(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*rareTermsSpec)
	vs, err := pc.resolve(d, 0, &spec.vs, "rare_terms", vsBytes, termsKinds...)
	if err != nil {
		return err
	}
	if vs.kind == vsNumeric && vs.floating {
		return errIllegalArgument("RareTerms aggregation does not support floating point fields.")
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

// rareTermsTyped maps the merged terms class (sterms/lterms) to
// RareTermsAggregatorFactory's two result types; dterms cannot occur since
// floating point fields are rejected at prepare time.
func rareTermsTyped(typed string) (string, string) {
	if typed == "lterms" {
		return "lrareterms", "LongRareTerms"
	}
	return "srareterms", "StringRareTerms"
}

func collectRareTerms(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*rareTermsSpec)
	typed, _, err := ac.termsClass(d, 0)
	if err != nil {
		return nil, err
	}
	groups := map[termKey]*bucket{}
	var keys []termKey
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil || (vs.unmapped && vs.missing == nil) {
			continue
		}
		accept, _ := ac.aux(d, 0, h.ix).(func(termKey) bool)
		for _, k := range termValues(vs, h) {
			if accept != nil && !accept(k) {
				continue
			}
			b, ok := groups[k]
			if !ok {
				b = &bucket{sortKey: k}
				groups[k] = b
				keys = append(keys, k)
			}
			b.docCount++
			b.hits = append(b.hits, h)
		}
	}
	final := make([]*bucket, 0, len(keys))
	for _, k := range keys {
		if b := groups[k]; b.docCount <= spec.maxDocCount {
			final = append(final, b)
		}
	}
	keyCmp := termsKeyCompare(ac, d)
	sort.SliceStable(final, func(i, j int) bool {
		if final[i].docCount != final[j].docCount {
			return final[i].docCount < final[j].docCount
		}
		return keyCmp(final[i], final[j]) < 0
	})
	if err := ac.collectSubs(d, final); err != nil {
		return nil, err
	}
	for _, b := range final {
		ac.renderTermKey(d, b, typed)
	}
	rTyped, javaClass := rareTermsTyped(typed)
	return &aggResult{kind: resBuckets, typed: rTyped, javaClass: javaClass, buckets: final}, nil
}
