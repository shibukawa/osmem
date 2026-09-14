package engine

import (
	"sort"
	"strings"
)

type fieldCapGroup struct {
	name            string
	mapped          map[string]bool
	nonSearchable   map[string]bool
	nonAggregatable map[string]bool
}

// FieldCaps implements GET/POST /_field_caps for mapped fields. It reports
// the mapping type and the index/doc-values capabilities available in osmem.
func (c *Cluster) FieldCaps(expr string, fields []string, includeUnmapped bool, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := body["index_filter"]; ok {
		return fail(errUnsupported("field_caps index_filter"))
	}
	targets, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	unique := make([]target, 0, len(targets))
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.ix.Name] {
			continue
		}
		seen[t.ix.Name] = true
		unique = append(unique, t)
	}
	indexNames := make([]string, 0, len(unique))
	for _, t := range unique {
		indexNames = append(indexNames, t.ix.Name)
	}
	sort.Strings(indexNames)
	byName := make(map[string]*Index, len(unique))
	for _, t := range unique {
		byName[t.ix.Name] = t.ix
	}

	patterns := make([]string, 0, len(fields))
	for _, field := range fields {
		patterns = append(patterns, splitList(field)...)
	}
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}
	fieldNames := map[string]bool{}
	for _, pattern := range patterns {
		for _, indexName := range indexNames {
			for _, name := range byName[indexName].Mapping.leafFields(pattern) {
				fieldNames[name] = true
			}
		}
		if includeUnmapped && !strings.ContainsAny(pattern, "*?") {
			fieldNames[pattern] = true
		}
	}

	fieldResult := M{}
	orderedFields := make([]string, 0, len(fieldNames))
	for name := range fieldNames {
		orderedFields = append(orderedFields, name)
	}
	sort.Strings(orderedFields)
	for _, name := range orderedFields {
		groups := map[string]*fieldCapGroup{}
		unmapped := &fieldCapGroup{name: "unmapped", mapped: map[string]bool{}, nonSearchable: map[string]bool{}, nonAggregatable: map[string]bool{}}
		for _, indexName := range indexNames {
			ix := byName[indexName]
			f, _, ok := ix.Mapping.resolve(name)
			if !ok {
				if includeUnmapped {
					unmapped.mapped[indexName] = true
				}
				continue
			}
			group := groups[f.Type]
			if group == nil {
				group = &fieldCapGroup{name: f.Type, mapped: map[string]bool{}, nonSearchable: map[string]bool{}, nonAggregatable: map[string]bool{}}
				groups[f.Type] = group
			}
			group.mapped[indexName] = true
			if !fieldSearchable(f) {
				group.nonSearchable[indexName] = true
			}
			if !fieldAggregatable(f) {
				group.nonAggregatable[indexName] = true
			}
		}
		if len(unmapped.mapped) > 0 {
			groups[unmapped.name] = unmapped
		}
		if len(groups) == 0 {
			continue
		}
		types := M{}
		groupNames := make([]string, 0, len(groups))
		for typ := range groups {
			groupNames = append(groupNames, typ)
		}
		sort.Strings(groupNames)
		for _, typ := range groupNames {
			group := groups[typ]
			searchable := typ != "unmapped" && len(group.nonSearchable) == 0
			aggregatable := typ != "unmapped" && len(group.nonAggregatable) == 0
			caps := M{
				"type":         typ,
				"searchable":   searchable,
				"aggregatable": aggregatable,
			}
			if len(group.mapped) < len(indexNames) {
				caps["indices"] = sortedKeys(group.mapped)
			}
			if len(group.nonSearchable) > 0 {
				caps["non_searchable_indices"] = sortedKeys(group.nonSearchable)
			}
			if len(group.nonAggregatable) > 0 {
				caps["non_aggregatable_indices"] = sortedKeys(group.nonAggregatable)
			}
			types[typ] = caps
		}
		fieldResult[name] = types
	}
	return ok(M{"indices": indexNames, "fields": fieldResult})
}

func fieldSearchable(f *Field) bool {
	return f.Index && f.Type != TypeObject && f.Type != TypeNested
}

func fieldAggregatable(f *Field) bool {
	if f.Type == TypeText {
		return getBool(f.Extra, "fielddata", false)
	}
	if f.Type == TypeObject || f.Type == TypeNested || f.Type == TypeBinary || f.Type == TypeGeoShape || f.Type == TypeCompletion {
		return false
	}
	defaultValue := f.Type != TypeText && f.Type != TypeObject && f.Type != TypeNested && f.Type != TypeBinary && f.Type != TypeGeoShape && f.Type != TypeCompletion
	return getBool(f.Extra, "doc_values", defaultValue)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
