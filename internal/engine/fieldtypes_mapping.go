package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Mapping semantics shared by document building and value lookups:
// dynamic modes with templates, copy_to targets, nested objects included in
// their parents and the position checks of text fields.

// errStrictDynamicMode is the rejection of a field a strict dynamic mode
// does not introduce.
func errStrictDynamicMode(key, prefix, mode string) *Error {
	parent := strings.TrimSuffix(prefix, ".")
	if parent == "" {
		parent = "_doc"
	}
	return &Error{Status: 400, Type: "strict_dynamic_mapping_exception", Reason: "mapping set to " + mode + ", dynamic introduction of [" + key + "] within [" + parent + "] is not allowed"}
}

// textPositionChecks are the checks TextFieldMapper.Builder applies to the
// options that need positions.
func textPositionChecks(name string, f *Field) *Error {
	opts := getString(f.Extra, "index_options")
	positions := f.Index && opts != "docs" && opts != "freqs"
	if raw, ok := f.Extra["position_increment_gap"]; ok && !positions {
		if n, _ := toFloat(raw); n != -1 {
			return errIllegalArgument("Cannot set position_increment_gap on field [%s] without positions enabled", name)
		}
	}
	if _, ok := f.Extra["index_prefixes"]; ok && !f.Index {
		return errIllegalArgument("Cannot set index_prefixes on unindexed field [%s]", name)
	}
	if getBool(f.Extra, "index_phrases", false) {
		if !f.Index {
			return errIllegalArgument("Cannot set index_phrases on unindexed field [%s]", name)
		}
		if !positions {
			return errIllegalArgument("Cannot set index_phrases on field [%s] if positions are not enabled", name)
		}
	}
	return nil
}

// countJoinFields counts the join fields of a mapping.
func countJoinFields(fields map[string]*Field) int {
	n := 0
	for _, f := range fields {
		if f.Type == TypeJoin {
			n++
		}
		n += countJoinFields(f.Properties)
	}
	return n
}

// fixRedundantIncludes drops include_in_root from nested fields whose
// parent chain already includes them in the root document
// (DocumentMapper.fixRedundantIncludes).
func fixRedundantIncludes(fields map[string]*Field, parentIncluded bool) {
	for _, f := range fields {
		nested := f.Type == TypeNested
		viaParent := parentIncluded && nested && getBool(f.Extra, "include_in_parent", false)
		inRoot := nested && getBool(f.Extra, "include_in_root", false)
		if viaParent && inRoot {
			delete(f.Extra, "include_in_root")
		}
		fixRedundantIncludes(f.Properties, viaParent || inRoot)
	}
}

// visibleAt reports whether the values of a field path are part of the
// documents of a nested level: the level of the field itself, or an
// enclosing level its nested objects are included in.
func (m *Mapping) visibleAt(path, level string) bool {
	chain := m.nestedChain(path)
	ancestor := ""
	if len(chain) > 0 {
		ancestor = chain[len(chain)-1]
	}
	if ancestor == level {
		return true
	}
	for i := len(chain) - 1; i >= 0; i-- {
		nf := m.fieldAtPath(chain[i])
		if nf == nil {
			return false
		}
		if level == "" && getBool(nf.Extra, "include_in_root", false) {
			return true
		}
		if !getBool(nf.Extra, "include_in_parent", false) {
			return false
		}
		parent := ""
		if i > 0 {
			parent = chain[i-1]
		}
		if parent == level {
			return true
		}
	}
	return false
}

// copyToSources lists the fields that copy their values to target.
func (m *Mapping) copyToSources(target string) []string {
	var out []string
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for _, name := range sortedFieldNames(fields) {
			f := fields[name]
			full := prefix + name
			for _, t := range f.CopyTo {
				if t == target {
					out = append(out, full)
					break
				}
			}
			walk(full+".", f.Properties)
		}
	}
	walk("", m.Properties)
	return out
}

// copiedValues returns the source values other fields copy into path for a
// document (they are part of its doc values but not of its source).
func (ix *Index) copiedValues(d *Doc, path string) []any {
	var out []any
	for _, src := range ix.Mapping.copyToSources(path) {
		sf, base, ok := ix.Mapping.resolve(src)
		if !ok {
			continue
		}
		raw, found := lookupPathFound(d.Src, base)
		if !found {
			continue
		}
		for _, v := range leafValues(sf, raw) {
			if v != nil {
				out = append(out, v)
			}
		}
	}
	return out
}

// pendingField returns a field this document introduced dynamically.
func (b *docBuilder) pendingField(path string) *Field {
	for i := len(b.pending) - 1; i >= 0; i-- {
		if b.pending[i].path == path {
			return b.pending[i].field
		}
	}
	return nil
}

// includeNested adds the fields of a nested object to the documents it is
// included in (include_in_parent, include_in_root).
func (b *docBuilder) includeNested(full string, f *Field, obj M, dynamic string) error {
	inParent := getBool(f.Extra, "include_in_parent", false)
	inRoot := getBool(f.Extra, "include_in_root", false)
	root := b.rootBuilder()
	var targets []*docBuilder
	if inParent {
		targets = append(targets, b)
	}
	if inRoot && !(inParent && b == root) {
		targets = append(targets, root)
	}
	for _, t := range targets {
		sb := &docBuilder{ix: b.ix, src: b.src, id: t.id, level: t.level, doc: t.doc, exists: t.exists, root: root, parent: t.parent,
			shadow: true, occ: map[string]int{}, body: b.body}
		if err := sb.walkObject(full+".", obj, f.Properties, dynamic, f.Dynamic, nil); err != nil {
			return err
		}
	}
	return nil
}

// copyToBuilder returns the builder of the document a copy_to target
// belongs to: the current nested object or the enclosing one of the target.
func (b *docBuilder) copyToBuilder(target string) *docBuilder {
	level := b.ix.Mapping.nestedAncestor(target)
	for cur := b; cur != nil; cur = cur.parent {
		if cur.level == level {
			return cur
		}
	}
	return b.rootBuilder()
}

// dynamicCopyTarget maps a copy_to target missing from the mapping the way
// the document parser introduces a dynamic field (and its parent objects)
// under the dynamic setting of the closest existing object. It returns nil
// when the field is not introduced.
func (b *docBuilder) dynamicCopyTarget(target string, value any) (*Field, error) {
	m := b.ix.Mapping
	parts := strings.Split(target, ".")
	fields := m.Properties
	dynamic := m.Dynamic
	prefix := ""
	i := 0
	for ; i < len(parts)-1; i++ {
		obj, ok := fields[parts[i]]
		if !ok {
			break
		}
		if obj.Type != TypeObject && obj.Type != TypeNested {
			return nil, nil
		}
		if obj.Dynamic != "" {
			dynamic = obj.Dynamic
		}
		fields = obj.Properties
		prefix += parts[i] + "."
	}
	check := prefix
	for j := i; j < len(parts); j++ {
		name := parts[j]
		switch dynamic {
		case "false":
			return nil, nil
		case "strict":
			return nil, errStrictDynamicMode(name, check, dynamic)
		case "strict_allow_templates", "false_allow_templates":
			var probe any = M{}
			if j == len(parts)-1 {
				probe = value
			}
			_, matched, err := m.templateField(check+name, probe)
			if err != nil {
				return nil, err
			}
			if !matched {
				if dynamic == "strict_allow_templates" {
					return nil, errStrictDynamicMode(name, check, dynamic)
				}
				return nil, nil
			}
		}
		check += name + "."
	}
	f, err := m.inferTreeForPath(target, value)
	if c, isConflict := err.(*dynamicTypeConflict); isConflict {
		return nil, b.conflictError(c)
	}
	if err != nil || f == nil {
		return nil, err
	}
	fields = m.Properties
	for _, p := range parts[:len(parts)-1] {
		obj, ok := fields[p]
		if !ok {
			obj = &Field{Type: TypeObject, Index: true, Enabled: true, Properties: map[string]*Field{}, inferred: true}
			fields[p] = obj
		}
		if obj.Properties == nil {
			obj.Properties = map[string]*Field{}
		}
		fields = obj.Properties
	}
	fields[parts[len(parts)-1]] = f
	return f, nil
}

// leafFailure reports a value a field could not parse, unless the index
// ignores malformed values of mappers without their own ignore_malformed
// parameter.
func (b *docBuilder) leafFailure(name string, f *Field, v any, preview string, cause *Error) (bool, error) {
	if b.ix.swallowsFailures(f) {
		return false, nil
	}
	return false, b.valueError(name, f, v, preview, cause)
}

// fieldsDateSourceError is the failure of the fields option on a date field
// whose source holds a floating point number: the value fetcher formats the
// Java double and parses it back with the field format.
func (ix *Index) fieldsDateSourceError(d *Doc, path string, f *Field) *Error {
	df := f.Format
	if df == nil {
		df = ParseDateFormat(DefaultDateFormat)
	}
	for _, v := range ix.sourceLeafValues(d, path) {
		n, ok := v.(json.Number)
		if !ok || isIntToken(n.String()) {
			continue
		}
		if _, de := df.parseDate(javaJSONNumberText(n), false, time.UTC); de != nil {
			cause := de.cause()
			cause.Status = 400
			cause.Index = ix.Name
			return errSearchPhase(cause)
		}
	}
	return nil
}

func stringsContain(list []string, s string) bool { return slices.Contains(list, s) }

func sortStrings(list []string) { sort.Strings(list) }

// dynamicTypeConflict is a field whose values infer different types.
type dynamicTypeConflict struct{ path, first, second string }

func (c *dynamicTypeConflict) Error() string {
	return fmt.Sprintf("mapper [%s] cannot be changed from type [%s] to [%s]", c.path, c.first, c.second)
}

// conflictError reports a dynamic type conflict the way the dynamic mapping
// update does: the new mappers of a document are merged in name order, so
// the types read in document order only for the first field introduced.
func (b *docBuilder) conflictError(c *dynamicTypeConflict) *Error {
	for _, name := range b.dynamicFieldNames() {
		if name < c.path {
			return errIllegalArgument("mapper [%s] cannot be changed from type [%s] to [%s]", c.path, c.second, c.first)
		}
	}
	return errIllegalArgument("mapper [%s] cannot be changed from type [%s] to [%s]", c.path, c.first, c.second)
}

// dynamicFieldNames lists the full names of the fields the document being
// built introduces into the mapping.
func (b *docBuilder) dynamicFieldNames() []string {
	var out []string
	var walkNew func(path string, v any)
	walkNew = func(path string, v any) {
		switch t := v.(type) {
		case nil:
		case []any:
			for _, e := range t {
				walkNew(path, e)
			}
		case M:
			out = append(out, path)
			for k, e := range t {
				walkNew(path+"."+k, e)
			}
		default:
			out = append(out, path)
		}
	}
	var walk func(prefix string, obj M, fields map[string]*Field)
	walk = func(prefix string, obj M, fields map[string]*Field) {
		for k, v := range obj {
			f, ok := fields[k]
			if !ok {
				walkNew(prefix+k, v)
				continue
			}
			if f.Type == TypeObject || f.Type == TypeNested {
				for _, m := range nestedObjects(v) {
					walk(prefix+k+".", m, f.Properties)
				}
			}
		}
	}
	if b.src != nil {
		walk("", b.src.Src, b.ix.Mapping.Properties)
	}
	return out
}

// normalizeDynamicTemplates stores the match patterns given as lists the way
// OpenSearch keeps them (the list's string form).
func normalizeDynamicTemplates(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(list))
	for i, item := range list {
		entry, ok := item.(M)
		if !ok {
			out[i] = item
			continue
		}
		ne := M{}
		for name, raw := range entry {
			spec, ok := raw.(M)
			if !ok {
				ne[name] = raw
				continue
			}
			ns := M{}
			for k, sv := range spec {
				switch k {
				case "match", "unmatch", "path_match", "path_unmatch":
					if arr, isList := sv.([]any); isList {
						sv = javaValueString(arr)
					}
				}
				ns[k] = sv
			}
			ne[name] = ns
		}
		out[i] = ne
	}
	return out
}

// textAnalyzersJSON renders the analyzers of a text field: each analyzer is
// shown when it is configured or differs from the next one in the chain
// (index, search, search_quote).
func (f *Field) textAnalyzersJSON(out M) {
	index := f.Analyzer
	if index == "" {
		index = "default"
	}
	search := f.SearchAnalyzer
	if search == "" {
		search = index
	}
	quote := getString(f.Extra, "search_quote_analyzer")
	quoteSet := quote != ""
	if quote == "" {
		quote = search
	}
	delete(out, "search_quote_analyzer")
	if (f.Analyzer != "" && f.Analyzer != "default") || index != search || index != quote {
		out["analyzer"] = index
	}
	if (f.SearchAnalyzer != "" && f.SearchAnalyzer != index) || search != quote {
		out["search_analyzer"] = search
	}
	if quoteSet && quote != search {
		out["search_quote_analyzer"] = quote
	}
}
