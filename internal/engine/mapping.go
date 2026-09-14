package engine

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Field types.
const (
	TypeText            = "text"
	TypeKeyword         = "keyword"
	TypeLong            = "long"
	TypeInteger         = "integer"
	TypeShort           = "short"
	TypeByte            = "byte"
	TypeDouble          = "double"
	TypeFloat           = "float"
	TypeHalfFloat       = "half_float"
	TypeScaledFloat     = "scaled_float"
	TypeUnsignedLong    = "unsigned_long"
	TypeBoolean         = "boolean"
	TypeDate            = "date"
	TypeDateNanos       = "date_nanos"
	TypeObject          = "object"
	TypeNested          = "nested"
	TypeGeoPoint        = "geo_point"
	TypeIP              = "ip"
	TypeBinary          = "binary"
	TypeConstantKeyword = "constant_keyword"
	TypeWildcard        = "wildcard"
	TypeSearchAsYouType = "search_as_you_type"
	TypeKNNVector       = "knn_vector"
	TypeRankFeature     = "rank_feature"
	TypeFlatObject      = "flat_object"
	TypeAlias           = "alias"
	TypeCompletion      = "completion"
	TypeTokenCount      = "token_count"
	TypeVersion         = "version"
	TypeIntegerRange    = "integer_range"
	TypeLongRange       = "long_range"
	TypeFloatRange      = "float_range"
	TypeDoubleRange     = "double_range"
	TypeDateRange       = "date_range"
	TypePercolator      = "percolator"
	TypeJoin            = "join"
	TypeGeoShape        = "geo_shape"
)

// Field is a mapped field.
type Field struct {
	Type           string
	Analyzer       string
	SearchAnalyzer string
	Normalizer     string
	normalizerSet  bool
	Index          bool // false when "index": false
	indexSet       bool
	Format         *DateFormat
	IgnoreAbove    int
	ignoreAboveSet bool
	NullValue      any
	nullValueSet   bool
	CopyTo         []string
	Dynamic        string // object/nested: "true", "false", "strict", "" (inherit)
	Enabled        bool   // object: enabled=false means not indexed
	Properties     map[string]*Field
	Fields         map[string]*Field // multi-fields
	Path           string            // alias target
	Extra          M                 // parameters echoed back verbatim
	inferred       bool
}

// Mapping is the mapping of an index.
type Mapping struct {
	Dynamic          string // "true", "false", "strict"
	Properties       map[string]*Field
	Extra            M // _meta, _source, _routing, dynamic_templates, ...
	DateDetection    bool
	NumericDetection bool
	DateFormats      *DateFormat
	initialized      bool // true after the initial mapping has been parsed
}

func newMapping() *Mapping {
	return &Mapping{Dynamic: "true", Properties: map[string]*Field{}, Extra: M{}, DateDetection: true}
}

func (m *Mapping) clone() *Mapping {
	n := &Mapping{Dynamic: m.Dynamic, Properties: cloneFields(m.Properties), Extra: cloneMap(m.Extra), DateDetection: m.DateDetection, NumericDetection: m.NumericDetection, DateFormats: m.DateFormats, initialized: m.initialized}
	return n
}

func cloneFields(src map[string]*Field) map[string]*Field {
	if src == nil {
		return nil
	}
	out := make(map[string]*Field, len(src))
	for k, f := range src {
		out[k] = f.clone()
	}
	return out
}

func (f *Field) clone() *Field {
	n := *f
	n.Properties = cloneFields(f.Properties)
	n.Fields = cloneFields(f.Fields)
	n.Extra = cloneMap(f.Extra)
	n.CopyTo = append([]string(nil), f.CopyTo...)
	return &n
}

func cloneMap(m M) M {
	if m == nil {
		return nil
	}
	out := make(M, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// parseMapping parses a "mappings" body. The body may be wrapped in "_doc"
// (legacy) or be the bare mapping object.
func parseMapping(body M) (*Mapping, error) {
	m := newMapping()
	if err := m.merge(body); err != nil {
		return nil, err
	}
	m.initialized = true
	return m, nil
}

// merge applies a mapping body on top of the existing mapping (PUT _mapping).
func (m *Mapping) merge(body M) error {
	if body == nil {
		return nil
	}
	if doc, ok := body["_doc"].(M); ok && len(body) == 1 {
		body = doc
	}
	if m.initialized {
		currentEnabled := getBool(m.Extra, "enabled", true)
		requestedEnabled := getBool(body, "enabled", true)
		if currentEnabled != requestedEnabled {
			return errMapperException("the [enabled] parameter can't be updated for the object mapping []")
		}
	}
	for k, v := range body {
		switch k {
		case "properties":
			props, ok := v.(M)
			if !ok {
				return errMapperParsing("properties must be an object")
			}
			if err := mergeProperties(m.Properties, props, ""); err != nil {
				return err
			}
		case "dynamic":
			dynamic, err := parseDynamicValue(v)
			if err != nil {
				return err
			}
			m.Dynamic = dynamic
		case "enabled":
			m.Extra[k] = v
		case "date_detection":
			m.DateDetection = getBool(body, "date_detection", true)
			m.Extra[k] = v
		case "numeric_detection":
			m.NumericDetection = getBool(body, "numeric_detection", false)
			m.Extra[k] = v
		case "dynamic_date_formats":
			m.DateFormats = ParseDateFormat(strings.Join(getStrings(body, k), "||"))
			m.Extra[k] = v
		default:
			m.Extra[k] = v
		}
	}
	m.initialized = true
	return nil
}

func dynamicValue(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		switch strings.ToLower(t) {
		case "true", "false", "strict":
			return strings.ToLower(t)
		}
	}
	return ""
}

func parseDynamicValue(v any) (string, error) {
	value := dynamicValue(v)
	if value == "" {
		return "", errMapperParsing("unknown value [%v] for dynamic", v)
	}
	return value, nil
}

func mergeProperties(dst map[string]*Field, props M, prefix string) error {
	for name, raw := range props {
		spec, ok := raw.(M)
		if !ok {
			return errMapperParsing("Expected map for property [fields] on field [%s] but got a class of %T", name, raw)
		}
		nf, err := parseField(name, spec)
		if err != nil {
			return err
		}
		full := prefix + name
		if existing, ok := dst[name]; ok {
			if err := existing.mergeWith(nf, full); err != nil {
				return err
			}
			continue
		}
		dst[name] = nf
	}
	return nil
}

func (f *Field) mergeWith(nf *Field, full string) error {
	if f.Type != nf.Type {
		if f.inferred && f.Type == TypeObject && nf.Type == TypeNested {
			f.Type = TypeNested
		} else {
			return errIllegalArgument("mapper [%s] cannot be changed from type [%s] to [%s]", full, f.Type, nf.Type)
		}
	}
	if f.Type == TypeObject || f.Type == TypeNested {
		if f.Enabled != nf.Enabled {
			return errMapperException("the [enabled] parameter can't be updated for the object mapping [%s]", full)
		}
		if f.Type == TypeNested {
			for _, parameter := range []string{"include_in_parent", "include_in_root"} {
				current := getBool(f.Extra, parameter, false)
				updated := getBool(nf.Extra, parameter, false)
				if current != updated {
					return errMapperException("the [%s] parameter can't be updated on a nested object mapping", parameter)
				}
			}
		}
		if nf.Dynamic != "" {
			f.Dynamic = nf.Dynamic
		}
		if f.Properties == nil {
			f.Properties = map[string]*Field{}
		}
		for k, v := range nf.Properties {
			if ex, ok := f.Properties[k]; ok {
				if err := ex.mergeWith(v, full+"."+k); err != nil {
					return err
				}
			} else {
				f.Properties[k] = v
			}
		}
		return nil
	}
	if f.Analyzer != nf.Analyzer && !(f.inferred && nf.Analyzer == "") {
		if nf.Analyzer != "" || f.inferred {
			return errIllegalArgument("Mapper for [%s] conflicts with existing mapper:\n\tCannot update parameter [analyzer] from [%s] to [%s]", full, f.Analyzer, nf.Analyzer)
		}
	}
	if nf.normalizerSet && f.Normalizer != nf.Normalizer {
		return errIllegalArgument("Mapper for [%s] conflicts with existing mapper:\n\tCannot update parameter [normalizer] from [%s] to [%s]", full, nullableMappingParameter(f.Normalizer), nullableMappingParameter(nf.Normalizer))
	}
	if requested, ok := nf.Extra["norms"]; ok {
		current := getBool(f.Extra, "norms", f.Type == TypeText)
		updated := getBool(M{"norms": requested}, "norms", current)
		if !current && updated {
			return errIllegalArgument("Cannot update parameter [norms] from [%t] to [%t] on field [%s]", current, updated, full)
		}
	}
	if nf.indexSet && f.Index != nf.Index {
		return errImmutableFieldParameter(full, "index")
	}
	for _, parameter := range []string{"doc_values", "store", "index_options", "null_value", "similarity", "term_vector"} {
		_, ok := nf.Extra[parameter]
		if parameter == "null_value" {
			ok = nf.nullValueSet
		}
		if !ok {
			continue
		}
		current := mappingParameterValue(f, parameter)
		updated := mappingParameterValue(nf, parameter)
		if !reflect.DeepEqual(current, updated) {
			return errImmutableFieldParameter(full, parameter)
		}
	}
	// updatable parameters
	if nf.ignoreAboveSet {
		f.IgnoreAbove = nf.IgnoreAbove
		f.ignoreAboveSet = true
	}
	if nf.SearchAnalyzer != "" {
		f.SearchAnalyzer = nf.SearchAnalyzer
	}
	for k, v := range nf.Fields {
		if f.Fields == nil {
			f.Fields = map[string]*Field{}
		}
		if _, ok := f.Fields[k]; !ok {
			f.Fields[k] = v
		}
	}
	for k, v := range nf.Extra {
		if f.Extra == nil {
			f.Extra = M{}
		}
		f.Extra[k] = v
	}
	f.inferred = false
	return nil
}

func mappingParameterValue(f *Field, parameter string) any {
	if value, ok := f.Extra[parameter]; ok {
		return value
	}
	switch parameter {
	case "doc_values":
		return f.Type != TypeText && f.Type != TypeObject && f.Type != TypeNested && f.Type != TypeBinary && f.Type != TypeGeoShape && f.Type != TypeCompletion
	case "store":
		return false
	case "index_options":
		if f.Type == TypeText {
			return "positions"
		}
		return "docs"
	case "null_value":
		return f.NullValue
	case "similarity":
		return "BM25"
	case "term_vector":
		return "no"
	default:
		return nil
	}
}

func errImmutableFieldParameter(field, parameter string) *Error {
	return errIllegalArgument("Mapper for [%s] conflicts with existing mapper:\n\tCannot update parameter [%s]", field, parameter)
}

func nullableMappingParameter(value string) string {
	if value == "" {
		return "null"
	}
	return value
}

func parseField(name string, spec M) (*Field, error) {
	f := &Field{Index: true, Enabled: true, Extra: M{}}
	typ := getString(spec, "type")
	if typ == "" {
		if _, ok := spec["properties"]; ok {
			typ = TypeObject
		} else if len(spec) == 0 {
			typ = TypeObject
		} else {
			return nil, errMapperParsing("No type specified for field [%s]", name)
		}
	}
	f.Type = typ
	switch typ {
	case TypeText, TypeKeyword, TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat,
		TypeScaledFloat, TypeUnsignedLong, TypeBoolean, TypeDate, TypeDateNanos, TypeObject, TypeNested, TypeGeoPoint,
		TypeIP, TypeBinary, TypeConstantKeyword, TypeWildcard, TypeSearchAsYouType, TypeKNNVector, TypeRankFeature,
		TypeFlatObject, TypeAlias, TypeCompletion, TypeTokenCount, TypeVersion, TypeIntegerRange, TypeLongRange,
		TypeFloatRange, TypeDoubleRange, TypeDateRange, TypePercolator, TypeJoin, TypeGeoShape:
	default:
		return nil, errMapperParsing("No handler for type [%s] declared on field [%s]", typ, name)
	}
	for k, v := range spec {
		switch k {
		case "type":
		case "analyzer":
			f.Analyzer = getString(spec, k)
		case "search_analyzer":
			f.SearchAnalyzer = getString(spec, k)
		case "normalizer":
			f.Normalizer = getString(spec, k)
			f.normalizerSet = true
		case "index":
			f.Index = getBool(spec, k, true)
			f.indexSet = true
		case "enabled":
			f.Enabled = getBool(spec, k, true)
		case "format":
			f.Format = ParseDateFormat(getString(spec, k))
		case "ignore_above":
			f.IgnoreAbove = getInt(spec, k, 0)
			f.ignoreAboveSet = true
		case "null_value":
			f.NullValue = v
			f.nullValueSet = true
		case "copy_to":
			f.CopyTo = getStrings(spec, k)
		case "dynamic":
			dynamic, err := parseDynamicValue(v)
			if err != nil {
				return nil, err
			}
			f.Dynamic = dynamic
		case "path":
			f.Path = getString(spec, k)
		case "properties":
			props, ok := v.(M)
			if !ok {
				return nil, errMapperParsing("properties must be an object")
			}
			f.Properties = map[string]*Field{}
			if err := mergeProperties(f.Properties, props, name+"."); err != nil {
				return nil, err
			}
		case "fields":
			sub, ok := v.(M)
			if !ok {
				return nil, errMapperParsing("fields must be an object")
			}
			f.Fields = map[string]*Field{}
			for sn, sraw := range sub {
				sspec, ok := sraw.(M)
				if !ok {
					return nil, errMapperParsing("Expected map for property [fields] on field [%s]", sn)
				}
				sf, err := parseField(name+"."+sn, sspec)
				if err != nil {
					return nil, err
				}
				f.Fields[sn] = sf
			}
		default:
			f.Extra[k] = v
		}
	}
	if (typ == TypeDate || typ == TypeDateNanos) && f.Format == nil {
		f.Format = ParseDateFormat(DefaultDateFormat)
	}
	return f, nil
}

// toJSON renders the mapping as OpenSearch would return it.
func (m *Mapping) toJSON() M {
	out := M{}
	for k, v := range m.Extra {
		out[k] = v
	}
	if m.Dynamic != "true" {
		out["dynamic"] = m.Dynamic
	}
	if len(m.Properties) > 0 {
		out["properties"] = fieldsJSON(m.Properties)
	}
	return out
}

func fieldsJSON(fields map[string]*Field) M {
	out := M{}
	for name, f := range fields {
		out[name] = f.toJSON()
	}
	return out
}

func (f *Field) toJSON() M {
	out := M{}
	for k, v := range f.Extra {
		out[k] = v
	}
	if f.Type != TypeObject || len(f.Properties) == 0 {
		out["type"] = f.Type
	}
	if f.Analyzer != "" {
		out["analyzer"] = f.Analyzer
	}
	if f.SearchAnalyzer != "" {
		out["search_analyzer"] = f.SearchAnalyzer
	}
	if f.Normalizer != "" {
		out["normalizer"] = f.Normalizer
	}
	if !f.Index {
		out["index"] = false
	}
	if !f.Enabled {
		out["enabled"] = false
	}
	if f.Format != nil && (f.Type == TypeDate || f.Type == TypeDateNanos) && f.Format.Source != DefaultDateFormat {
		out["format"] = f.Format.Source
	}
	if f.ignoreAboveSet || f.IgnoreAbove != 0 {
		out["ignore_above"] = f.IgnoreAbove
	}
	if f.NullValue != nil {
		out["null_value"] = f.NullValue
	}
	if len(f.CopyTo) > 0 {
		out["copy_to"] = f.CopyTo
	}
	if f.Dynamic != "" {
		out["dynamic"] = f.Dynamic
	}
	if f.Path != "" {
		out["path"] = f.Path
	}
	if len(f.Properties) > 0 {
		out["properties"] = fieldsJSON(f.Properties)
	}
	if len(f.Fields) > 0 {
		out["fields"] = fieldsJSON(f.Fields)
	}
	return out
}

// resolve finds the field for a dotted path. Multi-fields ("title.keyword")
// resolve to the sub-field; the returned base is the path of the value in
// the source document.
func (m *Mapping) resolve(path string) (f *Field, base string, ok bool) {
	parts := strings.Split(path, ".")
	fields := m.Properties
	var cur *Field
	for i, p := range parts {
		nf, found := fields[p]
		if !found {
			if cur != nil && cur.Fields != nil && i == len(parts)-1 {
				if sf, ok := cur.Fields[p]; ok {
					return sf, strings.Join(parts[:i], "."), true
				}
			}
			return nil, "", false
		}
		cur = nf
		if cur.Type == TypeAlias && cur.Path != "" {
			rest := strings.Join(parts[i+1:], ".")
			target := cur.Path
			if rest != "" {
				target += "." + rest
			}
			return m.resolve(target)
		}
		fields = cur.Properties
	}
	return cur, path, true
}

// leafFields lists all leaf field paths (including multi-fields) matching a
// wildcard pattern.
func (m *Mapping) leafFields(pattern string) []string {
	var out []string
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for name, f := range fields {
			full := prefix + name
			if f.Type == TypeObject || f.Type == TypeNested {
				walk(full+".", f.Properties)
				continue
			}
			if wildcardMatch(pattern, full) {
				out = append(out, full)
			}
			for sn := range f.Fields {
				if wildcardMatch(pattern, full+"."+sn) {
					out = append(out, full+"."+sn)
				}
			}
		}
	}
	walk("", m.Properties)
	sort.Strings(out)
	return out
}

// wildcardMatch matches s against a pattern with '*' and '?'.
func wildcardMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	// simple backtracking matcher
	var match func(p, s string) bool
	match = func(p, s string) bool {
		for len(p) > 0 {
			switch p[0] {
			case '*':
				for i := 0; i <= len(s); i++ {
					if match(p[1:], s[i:]) {
						return true
					}
				}
				return false
			case '?':
				if len(s) == 0 {
					return false
				}
				p, s = p[1:], s[1:]
			default:
				if len(s) == 0 || p[0] != s[0] {
					return false
				}
				p, s = p[1:], s[1:]
			}
		}
		return len(s) == 0
	}
	return match(pattern, s)
}

// isNumeric reports whether the type is a numeric type.
func (f *Field) isNumeric() bool {
	switch f.Type {
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat, TypeScaledFloat, TypeUnsignedLong, TypeTokenCount:
		return true
	}
	return false
}

func (f *Field) isDate() bool { return f.Type == TypeDate || f.Type == TypeDateNanos }

func (f *Field) isKeywordLike() bool {
	switch f.Type {
	case TypeKeyword, TypeConstantKeyword, TypeWildcard, TypeIP, TypeVersion:
		return true
	}
	return false
}

func (f *Field) isIntegral() bool {
	switch f.Type {
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeUnsignedLong, TypeTokenCount:
		return true
	}
	return false
}

// inferField builds a dynamically mapped field for a JSON value.
func (m *Mapping) inferField(v any) *Field {
	f := &Field{Index: true, Enabled: true, inferred: true}
	switch t := v.(type) {
	case string:
		if m.DateDetection {
			df := m.DateFormats
			if df == nil {
				df = dynamicDateFormats
			}
			if _, err := df.ParseString(t); err == nil && looksLikeDate(t) {
				f.Type = TypeDate
				f.Format = df
				return f
			}
		}
		if m.NumericDetection {
			if _, err := json.Number(t).Int64(); err == nil {
				f.Type = TypeLong
				return f
			}
			if _, err := json.Number(t).Float64(); err == nil {
				f.Type = TypeFloat
				return f
			}
		}
		f.Type = TypeText
		f.Fields = map[string]*Field{"keyword": {Type: TypeKeyword, Index: true, Enabled: true, IgnoreAbove: 256}}
	case json.Number:
		if _, err := t.Int64(); err == nil {
			f.Type = TypeLong
		} else {
			f.Type = TypeFloat
		}
	case float64:
		if t == float64(int64(t)) {
			f.Type = TypeLong
		} else {
			f.Type = TypeFloat
		}
	case bool:
		f.Type = TypeBoolean
	case M:
		f.Type = TypeObject
		f.Properties = map[string]*Field{}
	default:
		return nil
	}
	return f
}

// inferTreeForPath applies dynamic_templates while inferring a document's
// mapping. Templates are checked in their declared order and the first match
// supplies the field mapping.
func (m *Mapping) inferTreeForPath(path string, v any) (*Field, error) {
	if f, matched, err := m.templateField(path, v); matched || err != nil {
		return f, err
	}
	switch value := v.(type) {
	case []any:
		var inferred *Field
		for _, item := range value {
			if item == nil {
				continue
			}
			next, err := m.inferTreeForPath(path, item)
			if err != nil {
				return nil, err
			}
			if next == nil {
				continue
			}
			if inferred == nil {
				inferred = next
				continue
			}
			if inferred.Type == TypeObject && next.Type == TypeObject {
				for name, child := range next.Properties {
					if _, ok := inferred.Properties[name]; !ok {
						inferred.Properties[name] = child
					}
				}
			}
		}
		return inferred, nil
	case M:
		f := &Field{Type: TypeObject, Index: true, Enabled: true, Properties: map[string]*Field{}, inferred: true}
		for name, childValue := range value {
			if childValue == nil {
				continue
			}
			child, err := m.inferTreeForPath(path+"."+name, childValue)
			if err != nil {
				return nil, err
			}
			if child != nil {
				f.Properties[name] = child
			}
		}
		return f, nil
	default:
		return m.inferField(v), nil
	}
}

func (m *Mapping) templateField(path string, value any) (*Field, bool, error) {
	templates, _ := m.Extra["dynamic_templates"].([]any)
	if len(templates) == 0 {
		return nil, false, nil
	}
	name := path
	if dot := strings.LastIndexByte(path, '.'); dot >= 0 {
		name = path[dot+1:]
	}
	typeName := m.dynamicMappingType(value)
	for _, raw := range templates {
		entry, ok := raw.(M)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(entry))
		for k := range entry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, templateName := range keys {
			spec, ok := entry[templateName].(M)
			if !ok || !dynamicTemplateMatches(spec, name, path, typeName) {
				continue
			}
			mapping, ok := spec["mapping"].(M)
			if !ok {
				return nil, true, errMapperParsing("dynamic template [%s] is missing a mapping", templateName)
			}
			resolved := cloneDeep(mapping).(M)
			replaceTemplateValues(resolved, name, path, typeName)
			field, err := parseField(name, resolved)
			if err != nil {
				return nil, true, err
			}
			field.inferred = true
			return field, true, nil
		}
	}
	return nil, false, nil
}

func (m *Mapping) dynamicMappingType(v any) string {
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if item != nil {
				return m.dynamicMappingType(item)
			}
		}
		return "null"
	}
	if _, ok := v.(M); ok {
		return "object"
	}
	f := m.inferField(v)
	if f == nil {
		return "null"
	}
	switch f.Type {
	case TypeText:
		return "string"
	case TypeFloat, TypeDouble, TypeHalfFloat, TypeScaledFloat:
		return "double"
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeUnsignedLong:
		return "long"
	case TypeDate, TypeDateNanos:
		return "date"
	case TypeBoolean:
		return "boolean"
	default:
		return f.Type
	}
}

func dynamicTemplateMatches(spec M, name, path, typeName string) bool {
	patternMode := getString(spec, "match_pattern")
	for _, rule := range []struct {
		key   string
		value string
		mode  string
	}{
		{"match_mapping_type", typeName, "wildcard"},
		{"match", name, patternMode},
		{"path_match", path, patternMode},
	} {
		if raw, exists := spec[rule.key]; exists && !templatePatternMatches(raw, rule.value, rule.mode) {
			return false
		}
	}
	for _, rule := range []struct {
		key   string
		value string
		mode  string
	}{
		{"unmatch_mapping_type", typeName, "wildcard"},
		{"unmatch", name, patternMode},
		{"path_unmatch", path, patternMode},
	} {
		if raw, exists := spec[rule.key]; exists && templatePatternMatches(raw, rule.value, rule.mode) {
			return false
		}
	}
	return true
}

func templatePatternMatches(raw any, value, mode string) bool {
	if raw == nil {
		return false
	}
	var patterns []string
	switch v := raw.(type) {
	case string:
		patterns = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				patterns = append(patterns, s)
			}
		}
	default:
		return false
	}
	for _, pattern := range patterns {
		if mode == "regex" {
			if matched, err := regexp.MatchString(pattern, value); err == nil && matched {
				return true
			}
		} else if wildcardMatch(pattern, value) {
			return true
		}
	}
	return false
}

func replaceTemplateValues(value any, name, path, typeName string) {
	switch v := value.(type) {
	case M:
		for k, item := range v {
			replaceTemplateValues(item, name, path, typeName)
			if s, ok := item.(string); ok {
				v[k] = replaceTemplateString(s, name, path, typeName)
			}
		}
	case []any:
		for i, item := range v {
			replaceTemplateValues(item, name, path, typeName)
			if s, ok := item.(string); ok {
				v[i] = replaceTemplateString(s, name, path, typeName)
			}
		}
	}
}

func replaceTemplateString(s, name, path, typeName string) string {
	s = strings.ReplaceAll(s, "{{name}}", name)
	s = strings.ReplaceAll(s, "{{path}}", path)
	return strings.ReplaceAll(s, "{{dynamic_type}}", typeName)
}

var dynamicDateFormats = ParseDateFormat("strict_date_optional_time||yyyy/MM/dd HH:mm:ss Z||yyyy/MM/dd Z")

func looksLikeDate(s string) bool {
	// avoid mapping plain numbers such as "2024" as dates
	if len(s) < 6 {
		return false
	}
	return strings.ContainsAny(s, "-/:T")
}
