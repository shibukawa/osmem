package engine

import (
	"sort"
	"strings"
)

// metadataFieldCaps are the metadata fields every index reports.
var metadataFieldCaps = []struct {
	name, typ                string
	searchable, aggregatable bool
}{
	{"_data_stream_timestamp", "_data_stream_timestamp", false, false},
	{"_doc_count", "long", false, false},
	{"_feature", "_feature", false, false},
	{"_field_names", "_field_names", true, false},
	{"_id", "_id", true, true},
	{"_ignored", "_ignored", true, false},
	{"_index", "_index", true, true},
	{"_nested_path", "_nested_path", true, false},
	{"_routing", "_routing", true, false},
	{"_seq_no", "_seq_no", true, true},
	{"_source", "_source", false, false},
	{"_version", "_version", false, false},
}

// fieldCapInfo is the capabilities of a field in one index.
type fieldCapInfo struct {
	typ          string
	searchable   bool
	aggregatable bool
	meta         M
}

// indexFieldCaps lists the capabilities of every field of an index by path:
// metadata fields, objects, leaves and multi-fields (aliases report their
// target).
func indexFieldCaps(ix *Index) map[string]fieldCapInfo {
	out := make(map[string]fieldCapInfo, len(metadataFieldCaps))
	for _, m := range metadataFieldCaps {
		out[m.name] = fieldCapInfo{typ: m.typ, searchable: m.searchable, aggregatable: m.aggregatable}
	}
	caps := func(path string, f *Field) fieldCapInfo {
		if f.Type == TypeAlias {
			if target, _, ok := ix.Mapping.resolve(path); ok {
				f = target
			}
		}
		return fieldCapInfo{typ: f.typeName(), searchable: fieldSearchable(f), aggregatable: fieldAggregatable(f), meta: getMap(f.Extra, "meta")}
	}
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for name, f := range fields {
			full := prefix + name
			if f.Type == TypeObject || f.Type == TypeNested {
				out[full] = fieldCapInfo{typ: f.Type}
				walk(full+".", f.Properties)
				continue
			}
			out[full] = caps(full, f)
			for sub, sf := range f.Fields {
				out[full+"."+sub] = caps(full+"."+sub, sf)
			}
			// search_as_you_type has implicit shingle and prefix subfields
			if f.Type == TypeSearchAsYouType {
				for _, sub := range saytSubfieldNames(f) {
					path := full + "." + sub
					if sf, _, ok := ix.Mapping.resolve(path); ok {
						out[path] = caps(path, sf)
					}
				}
			}
		}
	}
	walk("", ix.Mapping.Properties)
	return out
}

// FieldCaps implements GET/POST /_field_caps
// (TransportFieldCapabilitiesAction): fields are grouped by type across the
// indices; the indices of a group are listed when a field has several
// types, the non-searchable and non-aggregatable indices when a group
// mixes them.
func (c *Cluster) FieldCaps(expr string, fields []string, includeUnmapped bool, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var filter *qnode
	if raw, ok := body["index_filter"]; ok {
		n, err := parseQuery(raw)
		if err != nil {
			return fail(err)
		}
		filter = n
	}
	targets, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandOpenOptions})
	if err != nil {
		return fail(err)
	}
	byName := map[string]*Index{}
	var indexNames []string
	for _, t := range targets {
		// a closed index has no shard to report capabilities
		if byName[t.ix.Name] != nil || t.ix.stateClosed {
			continue
		}
		if filter != nil && rewritesToMatchNone(filter, t.ix) {
			continue
		}
		byName[t.ix.Name] = t.ix
		indexNames = append(indexNames, t.ix.Name)
	}
	sort.Strings(indexNames)

	patterns := make([]string, 0, len(fields))
	for _, field := range fields {
		patterns = append(patterns, splitList(field)...)
	}
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}
	capsByIndex := make(map[string]map[string]fieldCapInfo, len(indexNames))
	fieldNames := map[string]bool{}
	for _, name := range indexNames {
		caps := indexFieldCaps(byName[name])
		capsByIndex[name] = caps
		for path := range caps {
			for _, pattern := range patterns {
				if wildcardMatch(pattern, path) {
					fieldNames[path] = true
					break
				}
			}
		}
	}
	ordered := make([]string, 0, len(fieldNames))
	for name := range fieldNames {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	type group struct {
		indices                  []string
		nonSearchable, nonAgg    []string
		searchable, aggregatable int
		meta                     map[string]map[string]bool
	}
	fieldResult := M{}
	for _, name := range ordered {
		groups := map[string]*group{}
		var typeOrder []string
		for _, indexName := range indexNames {
			info, ok := capsByIndex[indexName][name]
			if !ok {
				if !includeUnmapped {
					continue
				}
				info = fieldCapInfo{typ: "unmapped"}
			}
			g := groups[info.typ]
			if g == nil {
				g = &group{meta: map[string]map[string]bool{}}
				groups[info.typ] = g
				typeOrder = append(typeOrder, info.typ)
			}
			g.indices = append(g.indices, indexName)
			if info.searchable {
				g.searchable++
			} else {
				g.nonSearchable = append(g.nonSearchable, indexName)
			}
			if info.aggregatable {
				g.aggregatable++
			} else {
				g.nonAgg = append(g.nonAgg, indexName)
			}
			for k, v := range info.meta {
				if g.meta[k] == nil {
					g.meta[k] = map[string]bool{}
				}
				g.meta[k][xText(v)] = true
			}
		}
		types := M{}
		for _, typ := range typeOrder {
			g := groups[typ]
			caps := M{"type": typ, "searchable": len(g.nonSearchable) == 0, "aggregatable": len(g.nonAgg) == 0}
			if len(groups) > 1 {
				caps["indices"] = g.indices
			}
			if len(g.nonSearchable) > 0 && g.searchable > 0 {
				caps["non_searchable_indices"] = g.nonSearchable
			}
			if len(g.nonAgg) > 0 && g.aggregatable > 0 {
				caps["non_aggregatable_indices"] = g.nonAgg
			}
			if len(g.meta) > 0 {
				meta := M{}
				for k, values := range g.meta {
					list := make([]string, 0, len(values))
					for v := range values {
						list = append(list, v)
					}
					sort.Strings(list)
					meta[k] = list
				}
				caps["meta"] = meta
			}
			types[typ] = caps
		}
		if len(types) == 0 || (len(types) == 1 && types["unmapped"] != nil) {
			continue
		}
		fieldResult[name] = types
	}
	if indexNames == nil {
		indexNames = []string{}
	}
	return ok(M{"indices": indexNames, "fields": fieldResult})
}

// rewritesToMatchNone reports whether a query rewrites to match_none on an
// index from its mapping alone (the can_match rewrite index_filter uses):
// field queries on unmapped fields, and compound queries all of whose
// required parts do.
func rewritesToMatchNone(n *qnode, ix *Index) bool {
	unmapped := func(field string) bool {
		if strings.HasPrefix(field, "_") {
			for _, m := range metadataFieldCaps {
				if m.name == field {
					return false
				}
			}
		}
		_, _, ok := ix.Mapping.resolve(field)
		return !ok
	}
	switch spec := n.spec.(type) {
	case *termSpec:
		if spec.field == "_index" {
			return !indexMatchesName(ix, xText(spec.value))
		}
		return unmapped(spec.field)
	case *termsSpec:
		return spec.lookup == nil && unmapped(spec.field)
	case *rangeSpec:
		return unmapped(spec.field)
	case *multiTermSpec:
		return unmapped(spec.field)
	case *fuzzySpec:
		return unmapped(spec.field)
	case *existsSpec:
		return unmapped(spec.field)
	case *boolSpec:
		for _, q := range append(append([]*qnode(nil), spec.must...), spec.filter...) {
			if rewritesToMatchNone(q, ix) {
				return true
			}
		}
		if len(spec.must)+len(spec.filter) == 0 && len(spec.should) > 0 {
			for _, q := range spec.should {
				if !rewritesToMatchNone(q, ix) {
					return false
				}
			}
			return true
		}
		return false
	case *constantScoreSpec:
		return rewritesToMatchNone(spec.filter, ix)
	case *boostingSpec:
		return rewritesToMatchNone(spec.positive, ix)
	case *disMaxSpec:
		for _, q := range spec.queries {
			if !rewritesToMatchNone(q, ix) {
				return false
			}
		}
		return len(spec.queries) > 0
	}
	return n.kind == "match_none"
}

func fieldSearchable(f *Field) bool {
	return f.Index && f.Type != TypeObject && f.Type != TypeNested && f.Type != TypeBinary
}

func fieldAggregatable(f *Field) bool {
	if f.Type == TypeText {
		return getBool(f.Extra, "fielddata", false)
	}
	if f.Type == TypeObject || f.Type == TypeNested || f.Type == TypeGeoShape || f.Type == TypeCompletion || f.Type == TypeSearchAsYouType {
		return false
	}
	if f.Type == TypeBinary {
		// binary fields have no doc values unless the mapping enables them
		return getBool(f.Extra, "doc_values", false)
	}
	return getBool(f.Extra, "doc_values", true)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
