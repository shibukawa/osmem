package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
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
	TypeMatchOnlyText   = "match_only_text"
	TypeIPRange         = "ip_range"
	TypeRankFeatures    = "rank_features"
	TypeXYPoint         = "xy_point"
	TypeXYShape         = "xy_shape"
)

var knownFieldTypes = set(TypeText, TypeKeyword, TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat,
	TypeScaledFloat, TypeUnsignedLong, TypeBoolean, TypeDate, TypeDateNanos, TypeObject, TypeNested, TypeGeoPoint, TypeIP, TypeBinary,
	TypeConstantKeyword, TypeWildcard, TypeSearchAsYouType, TypeKNNVector, TypeRankFeature, TypeFlatObject, TypeAlias, TypeCompletion,
	TypeTokenCount, TypeVersion, TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange, TypeDateRange, TypePercolator, TypeJoin,
	TypeGeoShape, TypeMatchOnlyText, TypeIPRange, TypeRankFeatures, TypeXYPoint, TypeXYShape)

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
	Extra          M                 // other parameters, normalized
	inferred       bool
	// search_as_you_type subfields: the shingle size of a _<n>gram field or
	// the max_shingle_size of the _index_prefix field
	shingles   int
	saytPrefix bool
}

// Mapping is the mapping of an index.
type Mapping struct {
	Dynamic          string // "true", "false", "strict"
	dynamicSet       bool
	Properties       map[string]*Field
	Extra            M // _meta, _source, _routing, dynamic_templates, ...
	DateDetection    bool
	NumericDetection bool
	DateFormats      []*DateFormat // dynamic_date_formats
	dateFormatsSet   bool
	initialized      bool         // true after the initial mapping has been parsed
	analysis         *analysisSet // the index analysis (analyzer checks of PUT _mapping)
	settings         M
}

func newMapping() *Mapping {
	return &Mapping{Dynamic: "true", Properties: map[string]*Field{}, Extra: M{}, DateDetection: true}
}

func (m *Mapping) clone() *Mapping {
	return &Mapping{Dynamic: m.Dynamic, dynamicSet: m.dynamicSet, Properties: cloneFields(m.Properties), Extra: cloneMap(m.Extra),
		DateDetection: m.DateDetection, NumericDetection: m.NumericDetection, DateFormats: m.DateFormats, dateFormatsSet: m.dateFormatsSet,
		initialized: m.initialized, analysis: m.analysis, settings: m.settings}
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

// mapping errors ----------------------------------------------------------------

// mappingError marks the errors OpenSearch raises after parsing a mapping
// (not reported as "Failed to parse mapping").
type mappingError struct{ err *Error }

func (e *mappingError) Error() string { return e.err.Error() }

func noWrap(e *Error) error { return &mappingError{err: e} }

func asError(err error) *Error {
	var me *mappingError
	if errors.As(err, &me) {
		return me.err
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Status: 400, Type: "mapper_parsing_exception", Reason: err.Error()}
}

// errEnabledUpdate is the MapperException (HTTP 500) of changing enabled.
func errEnabledUpdate(name string) *Error {
	return &Error{Status: 500, Type: "mapper_exception", Reason: fmt.Sprintf("the [enabled] parameter can't be updated for the object mapping [%s]", name)}
}

func wrapFailedMapping(e *Error) *Error {
	return &Error{Status: 400, Type: "mapper_parsing_exception", Reason: "Failed to parse mapping [_doc]: " + e.Reason, Cause: e}
}

// parseMapping parses the "mappings" of a create index request.
func parseMapping(body M) (*Mapping, error) {
	m := newMapping()
	if err := m.mergeBody(body, true); err != nil {
		var me *mappingError
		if errors.As(err, &me) {
			return nil, me.err
		}
		return nil, wrapFailedMapping(asError(err))
	}
	m.initialized = true
	if err := m.validate(); err != nil {
		return nil, asError(err)
	}
	return m, nil
}

// merge applies a PUT _mapping body.
func (m *Mapping) merge(body M) error {
	if err := m.mergeBody(body, false); err != nil {
		return asError(err)
	}
	m.initialized = true
	if err := m.validate(); err != nil {
		return asError(err)
	}
	if m.analysis != nil {
		if err := m.validateAnalysis(m.analysis, m.settings); err != nil {
			return asError(err)
		}
	}
	return nil
}

func (m *Mapping) isEmpty() bool {
	return len(m.Properties) == 0 && !m.dynamicSet && len(m.Extra) == 0
}

func (m *Mapping) mergeBody(body M, create bool) error {
	if body == nil {
		return nil
	}
	if !create {
		if _, ok := body["_doc"]; ok {
			return noWrap(errIllegalArgument("Types cannot be provided in put mapping requests"))
		}
	} else if doc, ok := body["_doc"].(M); ok && len(body) == 1 {
		body = doc
	}
	if m.initialized && !create {
		// the root object mapper compares enabled even when the update omits it
		requested := true
		if raw, ok := body["enabled"]; ok {
			requested, _ = strictBool(raw)
		}
		if getBool(m.Extra, "enabled", true) != requested {
			return noWrap(errEnabledUpdate("_doc"))
		}
	}
	var unsupported []string
	for _, k := range javaHashMapOrder(sortedMapKeys(body)) {
		v := body[k]
		switch k {
		case "properties":
			switch props := v.(type) {
			case M:
				if err := m.mergeProperties(m.Properties, props, ""); err != nil {
					return err
				}
				if countJoinFields(m.Properties) > 1 {
					return errMapperParsing("Field [_parent_join] is defined more than once")
				}
			case []any:
				if len(props) > 0 {
					return &Error{Status: 400, Type: "parse_exception", Reason: "properties must be a map type"}
				}
			default:
				return noWrap(&Error{Status: 400, Type: "parse_exception", Reason: "properties must be a map type"})
			}
		case "dynamic":
			d, err := parseDynamicSetting(v)
			if err != nil {
				return err
			}
			m.Dynamic, m.dynamicSet = d, true
		case "enabled":
			b, err := convertBool(v, "enabled")
			if err != nil {
				return err
			}
			m.Extra[k] = b
		case "date_detection", "numeric_detection":
			b, err := convertBool(v, k)
			if err != nil {
				return err
			}
			if k == "date_detection" {
				m.DateDetection = b
			} else {
				m.NumericDetection = b
			}
			m.Extra[k] = b
		case "dynamic_date_formats":
			formats, list, err := parseDynamicDateFormats(v)
			if err != nil {
				return err
			}
			m.DateFormats, m.dateFormatsSet = formats, true
			m.Extra[k] = list
		case "dynamic_templates":
			if err := validateDynamicTemplates(v); err != nil {
				return err
			}
			m.Extra[k] = normalizeDynamicTemplates(v)
		case "_meta":
			if _, ok := v.(M); !ok {
				return classCastToMap(v)
			}
			m.Extra[k] = v
		case "_routing":
			rm, ok := v.(M)
			if !ok {
				return classCastToMap(v)
			}
			required := false
			if raw, ok := rm["required"]; ok {
				b, err := strictBool(raw)
				if err != nil {
					return err
				}
				required = b
			}
			if !create {
				if old := getBool(getMap(m.Extra, "_routing"), "required", false); old != required {
					return errIllegalArgument("Mapper for [_routing] conflicts with existing mapper:\n\tCannot update parameter [required] from [%t] to [%t]", old, required)
				}
			}
			m.Extra[k] = M{"required": required}
		case "_source":
			sm, ok := v.(M)
			if !ok {
				return classCastToMap(v)
			}
			if !create {
				old := getMap(m.Extra, "_source")
				if oe, ne := getBool(old, "enabled", true), getBool(sm, "enabled", true); oe != ne {
					return errIllegalArgument("Mapper for [_source] conflicts with existing mapper:\n\tCannot update parameter [enabled] from [%t] to [%t]", oe, ne)
				}
				for _, key := range []string{"includes", "excludes"} {
					if o, n := javaValueString(stringList(old[key])), javaValueString(stringList(sm[key])); o != n {
						return errIllegalArgument("Mapper for [_source] conflicts with existing mapper:\n\tCannot update parameter [%s] from [%s] to [%s]", key, o, n)
					}
				}
			}
			m.Extra[k] = v
		case "_field_names":
			m.Extra[k] = v
		case "_id", "_index", "_seq_no", "_version", "_primary_term", "_ignored", "_nested_path", "_data_stream_timestamp", "_doc_count":
			return errMapperParsing("%s is not configurable", k)
		case "derived":
			return errMapperParsing("[derived] fields are not supported by osmem")
		case "composite":
			return errMapperParsing("[composite] (star tree) fields are not supported by osmem")
		default:
			unsupported = append(unsupported, k)
		}
	}
	if len(unsupported) > 0 {
		return errMapperParsing("Root mapping definition has unsupported parameters: %s", remainingFields(body, unsupported))
	}
	return nil
}

func stringList(v any) []any {
	out := []any{}
	for _, s := range getList(v) {
		out = append(out, javaValueString(s))
	}
	return out
}

func classCastToMap(v any) *Error {
	name := javaClassName(v)
	return &Error{Status: 400, Type: "class_cast_exception",
		Reason: fmt.Sprintf("class %s cannot be cast to class java.util.Map (%s and java.util.Map are in module java.base of loader 'bootstrap')", name, name)}
}

// parseDynamicSetting parses the dynamic parameter.
func parseDynamicSetting(v any) (string, *Error) {
	if s, ok := v.(string); ok {
		switch mode := strings.ToLower(s); mode {
		case "strict", "strict_allow_templates", "false_allow_templates":
			return mode, nil
		}
	}
	b, err := convertBool(v, "dynamic.dynamic")
	if err != nil {
		return "", err
	}
	return strconv.FormatBool(b), nil
}

func parseDynamicDateFormats(v any) ([]*DateFormat, []any, *Error) {
	var list []any
	switch t := v.(type) {
	case []any:
		list = t
	default:
		list = []any{javaValueString(v)}
	}
	formats := make([]*DateFormat, 0, len(list))
	out := make([]any, 0, len(list))
	for _, item := range list {
		s := javaValueString(item)
		df, err := parseDateFormatChecked(s)
		if err != nil {
			return nil, nil, asError(err)
		}
		formats = append(formats, df)
		out = append(out, s)
	}
	return formats, out, nil
}

// properties ---------------------------------------------------------------------

func (m *Mapping) mergeProperties(dst map[string]*Field, props M, prefix string) error {
	for _, name := range javaHashMapOrder(sortedMapKeys(props)) {
		spec, ok := props[name].(M)
		if !ok {
			return noWrap(errIllegalArgument("Expect the field config at the path %s should be a map.", prefix+name))
		}
		parts := strings.Split(name, ".")
		for _, part := range parts {
			if part == "" {
				return errIllegalArgument("name cannot be empty string")
			}
		}
		nf, err := m.parseField(prefix+name, parts[len(parts)-1], spec, false)
		if err != nil {
			return err
		}
		for i := len(parts) - 2; i >= 0; i-- {
			nf = &Field{Type: TypeObject, Index: true, Enabled: true, Extra: M{}, Properties: map[string]*Field{parts[i+1]: nf}}
		}
		key := parts[0]
		if existing, ok := dst[key]; ok {
			if err := existing.mergeWith(nf, prefix+key); err != nil {
				return err
			}
			continue
		}
		dst[key] = nf
	}
	return nil
}

// parseField parses the definition of a field (a multi-field when multi).
func (m *Mapping) parseField(path, name string, spec M, multi bool) (*Field, error) {
	typ := ""
	if raw, ok := spec["type"]; ok {
		s, isString := raw.(string)
		if !isString {
			return nil, errMapperParsing("No handler for type [%s] declared on field [%s]", javaValueString(raw), name)
		}
		typ = s
	} else if _, ok := spec["properties"]; ok {
		typ = TypeObject
	} else if _, ok := spec["enabled"]; ok && len(spec) == 1 {
		typ = TypeObject
	} else {
		return nil, errMapperParsing("No type specified for field [%s]", name)
	}
	if multi && (typ == TypeObject || typ == TypeNested || typ == TypeAlias) {
		return nil, errMapperParsing("Type [%s] cannot be used in multi field", typ)
	}
	if !knownFieldTypes[typ] {
		return nil, errMapperParsing("No handler for type [%s] declared on field [%s]", typ, name)
	}
	if typ == TypeJoin && (multi || strings.Contains(path, ".")) {
		return nil, errIllegalArgument("join field [%s] cannot be added inside an object or in a multi-field", path)
	}
	f := &Field{Type: typ, Index: true, Enabled: true, Extra: M{}}
	var err error
	if accepted, old := oldStyleParams[typ]; old {
		err = m.parseOldStyle(path, name, f, spec, accepted)
	} else {
		err = m.parseParametrized(path, name, f, spec)
	}
	if err != nil {
		return nil, err
	}
	if err := f.checkParameters(name); err != nil {
		return nil, err
	}
	// multi-fields are built after their parent's checks
	for _, sn := range sortedFieldNames(f.Fields) {
		if len(f.Fields[sn].CopyTo) > 0 {
			return nil, noWrap(errIllegalArgument("[copy_to] may not be used to copy from a multi-field: [%s]", path+"."+sn))
		}
	}
	if f.isDate() && f.Format == nil {
		f.Format = ParseDateFormat(DefaultDateFormat)
	}
	return f, nil
}

func (m *Mapping) parseParametrized(path, name string, f *Field, spec M) error {
	specs := map[string]paramSpec{}
	for _, ps := range fieldParams[f.Type] {
		specs[ps.name] = ps
	}
	for _, k := range javaHashMapOrder(sortedMapKeys(spec)) {
		if k == "type" {
			continue
		}
		v := spec[k]
		switch k {
		case "fields":
			if err := m.parseMultiFields(path, name, f, v); err != nil {
				return err
			}
			continue
		case "copy_to":
			f.CopyTo = copyToTargets(v)
			continue
		}
		ps, ok := specs[k]
		if !ok {
			return errMapperParsing("unknown parameter [%s] on mapper [%s] of type [%s]", k, name, f.Type)
		}
		if err := f.applyParam(name, ps, v); err != nil {
			return err
		}
	}
	return nil
}

func copyToTargets(v any) []string {
	var out []string
	for _, item := range getList(v) {
		if item == nil {
			continue
		}
		out = append(out, javaValueString(item))
	}
	return out
}

func (m *Mapping) parseMultiFields(path, name string, f *Field, v any) error {
	sub, ok := v.(M)
	if !ok {
		return errMapperParsing("expected map for property [fields] on field [%s] or [fields] but got a class %s", javaValueString(v), javaClassName(v))
	}
	f.Fields = map[string]*Field{}
	for _, sn := range javaHashMapOrder(sortedMapKeys(sub)) {
		if strings.Contains(sn, ".") {
			return errMapperParsing("Field name [%s] which is a multi field of [%s] cannot contain '.'", sn, name)
		}
		sspec, ok := sub[sn].(M)
		if !ok {
			return errMapperParsing("expected map for property [fields] on field [%s] or [fields] but got a class %s", sn, javaClassName(sub[sn]))
		}
		sf, err := m.parseField(path+"."+sn, sn, sspec, true)
		if err != nil {
			return err
		}
		f.Fields[sn] = sf
	}
	return nil
}

func (f *Field) applyParam(name string, ps paramSpec, v any) error {
	if v == nil {
		switch ps.kind {
		case pkNullValue:
			return nil
		}
		return errMapperParsing("[%s] on mapper [%s] of type [%s] must not have a [null] value", ps.name, name, f.Type)
	}
	switch ps.kind {
	case pkBool:
		b, err := strictBool(v)
		if err != nil {
			return err
		}
		if ps.name == "index" {
			f.Index, f.indexSet = b, true
		} else {
			f.Extra[ps.name] = b
		}
	case pkInt:
		n, err := nodeInt(v)
		if err != nil {
			return err
		}
		if ps.name == "ignore_above" {
			f.IgnoreAbove, f.ignoreAboveSet = int(n), true
		} else {
			f.Extra[ps.name] = n
		}
	case pkFloat:
		d, err := nodeDouble(v)
		if err != nil {
			return err
		}
		f.Extra[ps.name] = Double(d)
	case pkEnum:
		s := javaValueString(v)
		found := false
		for _, allowed := range ps.enum {
			if allowed == s {
				found = true
			}
		}
		if !found {
			return errMapperParsing("Unknown value [%s] for field [%s] - accepted values are [%s]", s, ps.name, strings.Join(ps.enum, ", "))
		}
		f.Extra[ps.name] = s
	case pkMeta:
		if err := validateMeta(name, v); err != nil {
			return err
		}
		f.Extra[ps.name] = v
	case pkAnalyzer:
		s := javaValueString(v)
		switch ps.name {
		case "analyzer":
			f.Analyzer = s
		case "search_analyzer":
			f.SearchAnalyzer = s
		default:
			f.Extra[ps.name] = s
		}
	case pkNormalizer:
		f.Normalizer, f.normalizerSet = javaValueString(v), true
	case pkFormat:
		s := javaValueString(v)
		df, err := parseDateFormatChecked(s)
		if err != nil {
			inner := asError(err)
			return &Error{Status: 400, Type: "illegal_argument_exception", Reason: fmt.Sprintf("Error parsing [%s] on field [%s]: %s", ps.name, name, inner.Reason), Cause: inner}
		}
		if ps.name == "format" {
			f.Format = df
		} else {
			f.Extra[ps.name] = s
		}
	case pkNullValue:
		f.NullValue, f.nullValueSet = v, true
	case pkString, pkSimilarity:
		f.Extra[ps.name] = javaValueString(v)
	default:
		f.Extra[ps.name] = v
	}
	return nil
}

func (m *Mapping) parseOldStyle(path, name string, f *Field, spec M, accepted map[string]bool) error {
	var remaining []string
	for _, k := range javaHashMapOrder(sortedMapKeys(spec)) {
		if k == "type" {
			continue
		}
		v := spec[k]
		if !accepted[k] {
			remaining = append(remaining, k)
			continue
		}
		switch k {
		case "properties":
			f.Properties = map[string]*Field{}
			switch props := v.(type) {
			case M:
				if err := m.mergeProperties(f.Properties, props, path+"."); err != nil {
					return err
				}
			case []any:
			default:
				return &Error{Status: 400, Type: "parse_exception", Reason: "properties must be a map type"}
			}
		case "dynamic":
			d, err := parseDynamicSetting(v)
			if err != nil {
				return err
			}
			f.Dynamic = d
		case "enabled":
			b, err := convertBool(v, "enabled.enabled")
			if err != nil {
				return err
			}
			f.Enabled = b
		case "include_in_parent", "include_in_root":
			b, err := convertBool(v, name+"."+k)
			if err != nil {
				return err
			}
			f.Extra[k] = b
		case "path":
			f.Path = javaValueString(v)
		case "value":
			if v != nil {
				f.Extra[k] = javaValueString(v)
			}
		case "copy_to":
			f.CopyTo = copyToTargets(v)
		case "fields":
			if err := m.parseMultiFields(path, name, f, v); err != nil {
				return err
			}
		case "null_value":
			if v != nil {
				f.NullValue, f.nullValueSet = v, true
			}
		case "index":
			b, err := strictBool(v)
			if err != nil {
				return err
			}
			f.Index, f.indexSet = b, true
		case "ignore_malformed", "ignore_z_value", "doc_values", "store", "coerce", "points_only", "eager_global_ordinals":
			b, err := strictBool(v)
			if err != nil {
				return err
			}
			f.Extra[k] = b
		case "meta":
			if err := validateMeta(name, v); err != nil {
				return err
			}
			f.Extra[k] = v
		default:
			f.Extra[k] = v
		}
	}
	if len(remaining) > 0 {
		return errMapperParsing("Mapping definition for [%s] has unsupported parameters: %s", name, remainingFields(spec, remaining))
	}
	return nil
}

// checkParameters validates required parameters and typed null values.
func (f *Field) checkParameters(name string) error {
	switch f.Type {
	case TypeScaledFloat:
		raw, ok := f.Extra["scaling_factor"]
		if !ok {
			return errIllegalArgument("Field [scaling_factor] is required")
		}
		if d, _ := toFloat(raw); !(d > 0) || math.IsInf(d, 0) {
			return errIllegalArgument("[scaling_factor] must be a positive number, got [%s]", javaFloatText(d, 64))
		}
	case TypeTokenCount:
		if f.Analyzer == "" {
			return errMapperParsing("Analyzer must be set for field [%s] but wasn't.", name)
		}
	case TypeKNNVector:
		if _, ok := f.Extra["dimension"]; !ok {
			return errIllegalArgument("Dimension value missing for vector: %s", name)
		}
	case TypeConstantKeyword:
		if _, ok := f.Extra["value"]; !ok {
			return &Error{Status: 400, Type: "parse_exception", Reason: fmt.Sprintf("Field [%s] is missing required parameter [value]", name)}
		}
	case TypeText:
		if err := textPositionChecks(name, f); err != nil {
			return err
		}
	case TypeJoin:
		if err := normalizeJoinRelations(name, f); err != nil {
			return err
		}
	}
	if f.NullValue != nil {
		nv, err := normalizeNullValue(f)
		if err != nil {
			return err
		}
		f.NullValue = nv
	}
	return nil
}

// normalizeNullValue parses null_value as the field type does.
func normalizeNullValue(f *Field) (any, *Error) {
	v := f.NullValue
	switch f.Type {
	case TypeByte, TypeShort, TypeInteger, TypeLong, TypeUnsignedLong, TypeTokenCount:
		dec, err := hasDecimalPart(v)
		if err != nil {
			return nil, err
		}
		if dec {
			return nil, errIllegalArgument("Value [%s] has a decimal part", queryValueText(v))
		}
		n, err := parseIntegralQueryValue(f, v)
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case TypeByte:
			if n.Int64() < math.MinInt8 || n.Int64() > math.MaxInt8 {
				return nil, errIllegalArgument("Value [%d] is out of range for a byte", n.Int64())
			}
		case TypeShort:
			if n.Int64() < math.MinInt16 || n.Int64() > math.MaxInt16 {
				return nil, errIllegalArgument("Value [%d] is out of range for a short", n.Int64())
			}
		}
		return json.Number(n.String()), nil
	case TypeFloat, TypeHalfFloat, TypeDouble:
		d, err := parseFloatQueryValue(f, v)
		if err != nil {
			return nil, err
		}
		bits := 64
		if f.Type != TypeDouble {
			bits = 32
		}
		return json.Number(javaNumberString(d, bits)), nil
	case TypeScaledFloat:
		d, err := objectToDouble(v)
		if err != nil {
			return nil, err
		}
		return json.Number(javaNumberString(d, 64)), nil
	case TypeBoolean:
		b, err := strictBool(javaValueString(v))
		if err != nil {
			return nil, err
		}
		return b, nil
	case TypeGeoPoint:
		lat, lon, err := parseGeoPointValue(v, true)
		if err != nil {
			return nil, err
		}
		return M{"lat": Double(lat), "lon": Double(lon)}, nil
	case TypeKeyword, TypeWildcard, TypeIP, TypeDate, TypeDateNanos, TypeFlatObject:
		return javaValueString(v), nil
	}
	return v, nil
}

// validation after parsing -------------------------------------------------------

func (m *Mapping) validate() error {
	fixRedundantIncludes(m.Properties, true)
	var walk func(prefix string, fields map[string]*Field) error
	walk = func(prefix string, fields map[string]*Field) error {
		for _, name := range sortedFieldNames(fields) {
			f := fields[name]
			full := prefix + name
			if f.Type == TypeAlias {
				target := m.fieldAtPath(f.Path)
				switch {
				case f.Path == full:
					return noWrap(errMapperParsing("Invalid [path] value [%s] for field alias [%s]: an alias cannot refer to itself.", f.Path, full))
				case target == nil || target.Type == TypeObject || target.Type == TypeNested:
					return noWrap(errMapperParsing("Invalid [path] value [%s] for field alias [%s]: an alias must refer to an existing field in the mappings.", f.Path, full))
				case target.Type == TypeAlias:
					return noWrap(errMapperParsing("Invalid [path] value [%s] for field alias [%s]: an alias cannot refer to another alias.", f.Path, full))
				}
			}
			for _, target := range f.CopyTo {
				if tf := m.fieldAtPath(target); tf != nil && (tf.Type == TypeObject || tf.Type == TypeNested) {
					return noWrap(errIllegalArgument("Cannot copy to field [%s] since it is mapped as an object", target))
				}
				src, dst := m.nestedAncestor(full), m.nestedAncestor(target)
				if dst != "" && dst != src && !strings.HasPrefix(src+".", dst+".") {
					from := src
					if from == "" {
						from = "null"
					}
					return noWrap(errIllegalArgument("Illegal combination of [copy_to] and [nested] mappings: [copy_to] may only copy data to the current nested document or any of its parents, however one [copy_to] directive is trying to copy data from nested object [%s] to [%s]", from, dst))
				}
			}
			if err := walk(full+".", f.Properties); err != nil {
				return err
			}
		}
		return nil
	}
	return walk("", m.Properties)
}

// validateAnalysis checks the analyzers, normalizers and similarities
// fields refer to.
func (m *Mapping) validateAnalysis(as *analysisSet, settings M) error {
	similarities := getMap(getMap(settings, "index"), "similarity")
	var walk func(fields map[string]*Field) error
	walk = func(fields map[string]*Field) error {
		for _, name := range sortedFieldNames(fields) {
			f := fields[name]
			for _, an := range []string{f.Analyzer, f.SearchAnalyzer, getString(f.Extra, "search_quote_analyzer")} {
				if an == "" || an == "default" {
					continue
				}
				if _, err := as.analyzerNamed(an); err != nil {
					return errIllegalArgument("analyzer [%s] has not been configured in mappings", an)
				}
			}
			if f.Normalizer != "" {
				if _, err := as.normalizerNamed(f.Normalizer); err != nil {
					return errMapperParsing("normalizer [%s] not found for field [%s]", f.Normalizer, name)
				}
			}
			if s := getString(f.Extra, "similarity"); s != "" && !knownSimilarities[s] && similarities[s] == nil {
				return errMapperParsing("Unknown Similarity type [%s] for field [%s]", s, name)
			}
			if err := walk(f.Properties); err != nil {
				return err
			}
			if err := walk(f.Fields); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(m.Properties)
}

// merging (PUT _mapping) ------------------------------------------------------------

func (f *Field) mergeWith(nf *Field, full string) error {
	fObj := f.Type == TypeObject || f.Type == TypeNested
	nObj := nf.Type == TypeObject || nf.Type == TypeNested
	if fObj && nObj {
		if f.Type != nf.Type {
			if nf.Type == TypeNested {
				return errIllegalArgument("cannot change object mapping from non-nested to nested")
			}
			return errIllegalArgument("cannot change object mapping from nested to non-nested")
		}
		if f.Enabled != nf.Enabled {
			return errEnabledUpdate(full)
		}
		if f.Type == TypeNested {
			for _, parameter := range []string{"include_in_parent", "include_in_root"} {
				if getBool(f.Extra, parameter, false) != getBool(nf.Extra, parameter, false) {
					return &Error{Status: 500, Type: "mapper_exception", Reason: fmt.Sprintf("the [%s] parameter can't be updated on a nested object mapping", parameter)}
				}
			}
		}
		if nf.Dynamic != "" {
			f.Dynamic = nf.Dynamic
		}
		if f.Properties == nil {
			f.Properties = map[string]*Field{}
		}
		for _, k := range sortedFieldNames(nf.Properties) {
			v := nf.Properties[k]
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
	if fObj != nObj {
		return errIllegalArgument("can't merge a non object mapping [%s] with an object mapping", full)
	}
	if f.Type != nf.Type {
		return errIllegalArgument("mapper [%s] cannot be changed from type [%s] to [%s]", full, f.Type, nf.Type)
	}
	if f.Type == TypeJoin {
		if err := mergeJoinRelations(f, nf, full); err != nil {
			return err
		}
		f.inferred = false
		return nil
	}
	var conflicts []string
	for _, ps := range fieldParams[f.Type] {
		if ps.updatable {
			continue
		}
		oldV, newV := f.paramString(ps), nf.paramString(ps)
		if ps.name == "norms" && oldV == "true" && newV == "false" {
			continue
		}
		if oldV != newV {
			conflicts = append(conflicts, fmt.Sprintf("Cannot update parameter [%s] from [%s] to [%s]", ps.name, oldV, newV))
		}
	}
	if f.Type == TypeConstantKeyword {
		if o, n := getString(f.Extra, "value"), getString(nf.Extra, "value"); o != n {
			conflicts = append(conflicts, fmt.Sprintf("Cannot update parameter [value] from [%s] to [%s]", o, n))
		}
	}
	if len(conflicts) > 0 {
		return errIllegalArgument("Mapper for [%s] conflicts with existing mapper:\n\t%s", full, strings.Join(conflicts, "\n\t"))
	}
	if params, ok := fieldParams[f.Type]; ok {
		for _, ps := range params {
			if ps.updatable || ps.name == "norms" {
				f.setParamFrom(nf, ps)
			}
		}
	} else {
		value := f.Extra["value"]
		f.Extra = cloneMap(nf.Extra)
		if value != nil {
			f.Extra["value"] = value
		}
	}
	f.CopyTo = nf.CopyTo
	if f.Type == TypeAlias {
		f.Path = nf.Path
	}
	for _, k := range sortedFieldNames(nf.Fields) {
		sub := nf.Fields[k]
		if f.Fields == nil {
			f.Fields = map[string]*Field{}
		}
		if ex, ok := f.Fields[k]; ok {
			if err := ex.mergeWith(sub, full+"."+k); err != nil {
				return err
			}
		} else {
			f.Fields[k] = sub
		}
	}
	f.inferred = false
	return nil
}

// paramString renders a parameter value as conflict messages do.
func (f *Field) paramString(ps paramSpec) string {
	switch ps.name {
	case "index":
		return strconv.FormatBool(f.Index)
	case "analyzer":
		if f.Analyzer == "" {
			return ps.def
		}
		return f.Analyzer
	case "search_analyzer":
		if f.SearchAnalyzer == "" {
			return ps.def
		}
		return f.SearchAnalyzer
	case "normalizer":
		if f.Normalizer == "" {
			return "null"
		}
		return f.Normalizer
	case "format":
		if f.Format == nil {
			return ps.def
		}
		return f.Format.Source
	case "null_value":
		if f.NullValue == nil {
			return "null"
		}
		return paramDisplay(f.NullValue)
	case "ignore_above":
		if !f.ignoreAboveSet {
			return ps.def
		}
		return strconv.Itoa(f.IgnoreAbove)
	}
	v, ok := f.Extra[ps.name]
	if !ok {
		return ps.def
	}
	return paramDisplay(v)
}

func (f *Field) setParamFrom(nf *Field, ps paramSpec) {
	switch ps.name {
	case "search_analyzer":
		f.SearchAnalyzer = nf.SearchAnalyzer
	case "ignore_above":
		f.IgnoreAbove, f.ignoreAboveSet = nf.IgnoreAbove, nf.ignoreAboveSet
	default:
		if v, ok := nf.Extra[ps.name]; ok {
			if f.Extra == nil {
				f.Extra = M{}
			}
			f.Extra[ps.name] = v
		} else {
			delete(f.Extra, ps.name)
		}
	}
}

// rendering -----------------------------------------------------------------------

// toJSON renders the mapping as OpenSearch would return it.
func (m *Mapping) toJSON() M {
	out := M{}
	for k, v := range m.Extra {
		out[k] = v
	}
	if m.dynamicSet || m.Dynamic != "true" {
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
	if f.shingles > 0 {
		// the subfields of search_as_you_type fields
		typ := "shingle"
		if f.saytPrefix {
			typ = "prefix"
		}
		return M{"type": typ, "doc_values": false}
	}
	out := M{}
	specs := map[string]paramSpec{}
	for _, ps := range fieldParams[f.Type] {
		specs[ps.name] = ps
	}
	for k, v := range f.Extra {
		// parameters set to their default are not rendered, except the
		// explicit booleans that default to index settings
		if ps, ok := specs[k]; ok && k != "coerce" && k != "ignore_malformed" && ps.kind != pkMeta && paramDisplay(v) == ps.def {
			continue
		}
		out[k] = v
	}
	if dt, ok := out["data_type"].(string); ok && f.Type == TypeKNNVector {
		out["data_type"] = strings.ToUpper(dt)
	}
	if f.isNumeric() || f.Type == TypeIP {
		// numbers and ip addresses ignore boost
		delete(out, "boost")
	}
	if f.Type != TypeObject || len(f.Properties) == 0 {
		out["type"] = f.Type
	}
	switch f.Type {
	case TypeText, TypeMatchOnlyText, TypeSearchAsYouType:
		f.textAnalyzersJSON(out)
	default:
		if f.Analyzer != "" {
			out["analyzer"] = f.Analyzer
		}
		if f.SearchAnalyzer != "" {
			out["search_analyzer"] = f.SearchAnalyzer
		}
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
	if f.Format != nil && f.isDate() && f.Format.Source != DefaultDateFormat {
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
	defaults := func(key string, value any) {
		if _, ok := out[key]; !ok {
			out[key] = value
		}
	}
	switch f.Type {
	case TypeCompletion:
		defaults("analyzer", "simple")
		defaults("preserve_separators", true)
		defaults("preserve_position_increments", true)
		defaults("max_input_length", 50)
	case TypeSearchAsYouType:
		defaults("doc_values", false)
		defaults("max_shingle_size", 3)
	case TypeJoin:
		defaults("eager_global_ordinals", true)
	case TypeWildcard, TypeKNNVector:
		defaults("doc_values", true)
	case TypeVersion:
		defaults("index", true)
	}
	return out
}

// resolve finds the field for a dotted path. Multi-fields ("title.keyword")
// resolve to the sub-field; the returned base is the path of the value in
// the source document. Aliases resolve to their target; the sub-paths of a
// flat_object field resolve to keyword fields.
func (m *Mapping) resolve(path string) (f *Field, base string, ok bool) {
	if path == "_ignored" {
		return &Field{Type: TypeIgnoredMeta, Index: true, Enabled: true, Extra: M{"doc_values": false}}, path, true
	}
	if strings.IndexByte(path, '#') > 0 {
		if jf := m.joinParentField(path); jf != nil {
			return jf, path, true
		}
	}
	parts := strings.Split(path, ".")
	fields := m.Properties
	var cur *Field
	for i, p := range parts {
		nf, found := fields[p]
		if !found {
			if cur != nil && cur.Type == TypeFlatObject {
				return &Field{Type: TypeKeyword, Index: true, Enabled: true, Extra: M{}}, path, true
			}
			if cur != nil && cur.Fields != nil && i == len(parts)-1 {
				if sf, ok := cur.Fields[p]; ok {
					return sf, strings.Join(parts[:i], "."), true
				}
			}
			if cur != nil && cur.Type == TypeSearchAsYouType && i == len(parts)-1 {
				if sf := saytSubfield(cur, p); sf != nil {
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
			if target == path {
				return nil, "", false
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
			if f.Type == TypeSearchAsYouType {
				for _, sn := range saytSubfieldNames(f) {
					if wildcardMatch(pattern, full+"."+sn) {
						out = append(out, full+"."+sn)
					}
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

// simpleMatch is Regex.simpleMatch: '*' is the only wildcard.
func simpleMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
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

// dynamic mapping -----------------------------------------------------------------

var defaultDynamicDateFormats = []*DateFormat{
	ParseDateFormat(DefaultDateFormat),
	ParseDateFormat("yyyy/MM/dd HH:mm:ss||yyyy/MM/dd||epoch_millis"),
}

func (m *Mapping) dynamicDateFormatters() []*DateFormat {
	if m.dateFormatsSet {
		return m.DateFormats
	}
	return defaultDynamicDateFormats
}

// javaLongParseable is Long.parseLong succeeding.
func javaLongParseable(s string) bool {
	if !javaLongTokenRe.MatchString(s) {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// detectDate returns the dynamic date format that parses s, as dynamic
// date detection does for strings that are not numbers.
func (m *Mapping) detectDate(s string) *DateFormat {
	if !m.DateDetection || javaLongParseable(s) {
		return nil
	}
	if _, err := javaParseDouble(s, 64); err == nil {
		return nil
	}
	for _, df := range m.dynamicDateFormatters() {
		if _, err := df.ParseString(s); err == nil {
			return df
		}
	}
	return nil
}

func setDynamicDateFormat(f *Field, df *DateFormat) {
	f.Format = df
	if df.Source != DefaultDateFormat {
		first := strings.Split(strings.TrimPrefix(df.Source, "8"), "||")[0]
		f.Extra["print_format"] = first
	}
}

// inferField builds a dynamically mapped field for a JSON value.
func (m *Mapping) inferField(v any) *Field {
	f := &Field{Index: true, Enabled: true, inferred: true, Extra: M{}}
	switch t := v.(type) {
	case string:
		if m.NumericDetection {
			if javaLongParseable(t) {
				f.Type = TypeLong
				return f
			}
			if _, err := javaParseDouble(t, 64); err == nil {
				f.Type = TypeFloat
				return f
			}
		}
		if df := m.detectDate(t); df != nil {
			f.Type = TypeDate
			setDynamicDateFormat(f, df)
			return f
		}
		f.Type = TypeText
		f.Fields = map[string]*Field{"keyword": {Type: TypeKeyword, Index: true, Enabled: true, IgnoreAbove: 256, Extra: M{}}}
	case json.Number:
		if isIntToken(t.String()) {
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
	if arr, ok := v.([]any); ok {
		var inferred *Field
		for _, item := range arr {
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
			if err := mergeInferredFields(inferred, next, path, false); err != nil {
				return nil, err
			}
		}
		return inferred, nil
	}
	if f, matched, err := m.templateField(path, v); matched || err != nil {
		return f, err
	}
	if value, ok := v.(M); ok {
		f := &Field{Type: TypeObject, Index: true, Enabled: true, Properties: map[string]*Field{}, inferred: true, Extra: M{}}
		for _, name := range sortedMapKeys(value) {
			childValue := value[name]
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
	}
	return m.inferField(v), nil
}

// mergeInferredFields merges the mappers inferred for the values of an
// array, rejecting the conflicts OpenSearch reports.
func mergeInferredFields(a, b *Field, path string, inObject bool) error {
	aObj := a.Type == TypeObject || a.Type == TypeNested
	bObj := b.Type == TypeObject || b.Type == TypeNested
	if aObj != bObj {
		return errIllegalArgument("can't merge a non object mapping [%s] with an object mapping", path)
	}
	if aObj {
		for _, name := range sortedFieldNames(b.Properties) {
			child := b.Properties[name]
			if ex, ok := a.Properties[name]; ok {
				if err := mergeInferredFields(ex, child, path+"."+name, true); err != nil {
					return err
				}
			} else {
				a.Properties[name] = child
			}
		}
		return nil
	}
	if a.Type != b.Type {
		return &dynamicTypeConflict{path: path, first: a.Type, second: b.Type}
	}
	return nil
}

// dynamic templates ------------------------------------------------------------------

var dynamicTemplateKeys = set("match", "unmatch", "path_match", "path_unmatch", "match_pattern", "match_mapping_type", "mapping")

var dynamicMappingTypes = set("object", "string", "long", "double", "boolean", "date", "binary", "*")

// validateDynamicTemplates checks dynamic_templates like DynamicTemplate.parse.
func validateDynamicTemplates(v any) *Error {
	list, ok := v.([]any)
	if !ok {
		return errMapperParsing("Dynamic template syntax error. An array of named objects is expected.")
	}
	for _, item := range list {
		entry, ok := item.(M)
		if !ok {
			return classCastToMap(item)
		}
		if len(entry) != 1 {
			return errMapperParsing("A dynamic template must be defined with a name")
		}
		for _, raw := range entry {
			spec, ok := raw.(M)
			if !ok {
				return classCastToMap(raw)
			}
			for _, k := range javaHashMapOrder(sortedMapKeys(spec)) {
				if !dynamicTemplateKeys[k] {
					return errIllegalArgument("Illegal dynamic template parameter: [%s]", k)
				}
			}
			if _, ok := spec["mapping"]; !ok {
				return errMapperParsing("template must have mapping set")
			}
			if raw, ok := spec["match_mapping_type"]; ok {
				if s := javaValueString(raw); !dynamicMappingTypes[s] {
					return errIllegalArgument("No field type matched on [%s], possible values are [object, string, long, double, boolean, date, binary]", s)
				}
			}
			if raw, ok := spec["match_pattern"]; ok {
				if s := javaValueString(raw); s != "simple" && s != "regex" {
					return errIllegalArgument("No matching pattern matched on [%s]", s)
				}
			}
		}
	}
	return nil
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
	if typeName == "null" {
		return nil, false, nil
	}
	for _, raw := range templates {
		entry, ok := raw.(M)
		if !ok || len(entry) != 1 {
			continue
		}
		for _, specRaw := range entry {
			spec, ok := specRaw.(M)
			if !ok || !dynamicTemplateMatches(spec, name, path, typeName) {
				continue
			}
			mapping, ok := spec["mapping"].(M)
			if !ok {
				return nil, true, errMapperParsing("template must have mapping set")
			}
			defType := defaultDynamicType(typeName)
			resolved := replaceTemplateValues(cloneDeep(mapping), name, defType).(M)
			if _, ok := resolved["type"]; !ok {
				resolved["type"] = defType
			}
			if t, _ := resolved["type"].(string); t == TypeObject || t == TypeNested {
				// object mappers ignore the parameters they do not know when
				// they come from a dynamic template
				for k := range resolved {
					if k != "type" && !oldStyleParams[t][k] {
						delete(resolved, k)
					}
				}
			}
			field, err := m.parseField(path, name, resolved, false)
			if err != nil {
				return nil, true, errFailedToParse(asError(err))
			}
			if typeName == "date" && field.isDate() {
				if _, hasFormat := mapping["format"]; !hasFormat {
					if s, isString := value.(string); isString {
						if df := m.detectDate(s); df != nil {
							setDynamicDateFormat(field, df)
						}
					}
				}
			}
			field.inferred = true
			return field, true, nil
		}
	}
	return nil, false, nil
}

// dynamicMappingType is the XContentFieldType of a value.
func (m *Mapping) dynamicMappingType(v any) string {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if item != nil {
				return m.dynamicMappingType(item)
			}
		}
		return "null"
	case M:
		return "object"
	case bool:
		return "boolean"
	case json.Number:
		if isIntToken(t.String()) {
			return "long"
		}
		return "double"
	case float64:
		if t == float64(int64(t)) {
			return "long"
		}
		return "double"
	case string:
		if m.NumericDetection {
			if javaLongParseable(t) {
				return "long"
			}
			if _, err := javaParseDouble(t, 64); err == nil {
				return "double"
			}
		}
		if m.detectDate(t) != nil {
			return "date"
		}
		return "string"
	}
	return "null"
}

func defaultDynamicType(typeName string) string {
	switch typeName {
	case "string":
		return TypeText
	case "double":
		return TypeFloat
	}
	return typeName
}

func dynamicTemplateMatches(spec M, name, path, typeName string) bool {
	mode := getString(spec, "match_pattern")
	if raw, ok := spec["match_mapping_type"]; ok {
		if mt := javaValueString(raw); mt != "*" && mt != typeName {
			return false
		}
	}
	if raw, ok := spec["match"]; ok && !templatePatternMatches(raw, name, mode) {
		return false
	}
	if raw, ok := spec["path_match"]; ok && !templatePatternMatches(raw, path, mode) {
		return false
	}
	if raw, ok := spec["unmatch"]; ok && templatePatternMatches(raw, name, mode) {
		return false
	}
	if raw, ok := spec["path_unmatch"]; ok && templatePatternMatches(raw, path, mode) {
		return false
	}
	return true
}

func templatePatternMatches(raw any, value, mode string) bool {
	if raw == nil {
		return false
	}
	pattern := javaValueString(raw)
	if mode == "regex" {
		re, err := regexp.Compile("^(?:" + pattern + ")$")
		return err == nil && re.MatchString(value)
	}
	return simpleMatch(pattern, value)
}

// replaceTemplateValues substitutes {name} and {dynamic_type} in the keys
// and string values of a template mapping.
func replaceTemplateValues(value any, name, dynamicType string) any {
	replace := func(s string) string {
		s = strings.ReplaceAll(s, "{name}", name)
		s = strings.ReplaceAll(s, "{dynamic_type}", dynamicType)
		return strings.ReplaceAll(s, "{dynamicType}", dynamicType)
	}
	switch v := value.(type) {
	case M:
		out := make(M, len(v))
		for k, item := range v {
			out[replace(k)] = replaceTemplateValues(item, name, dynamicType)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = replaceTemplateValues(item, name, dynamicType)
		}
		return out
	case string:
		return replace(v)
	}
	return value
}
