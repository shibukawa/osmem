package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/document"
	"github.com/blevesearch/bleve/v2/index/scorch"
	index "github.com/blevesearch/bleve_index_api"
)

// Doc is a stored document. Docs are immutable once stored; updates create
// new Doc values so clones can share them.
type Doc struct {
	ID          string
	Raw         []byte // compact _source
	Src         M      // parsed _source (numbers as json.Number, dotted keys expanded)
	Version     int64
	SeqNo       int64
	PrimaryTerm int64
	// nested objects are represented by synthetic documents (see nested.go)
	nested []nestedLevel // identity of the object; nil for root documents
	obj    M             // the nested object itself
	root   *Doc          // the root document of a nested object
}

// Alias is an index alias definition.
type Alias struct {
	Filter        M
	IsWriteIndex  *bool
	Routing       string
	IndexRouting  string
	SearchRouting string
}

func (a *Alias) toJSON() M {
	out := M{}
	if a.Filter != nil {
		out["filter"] = a.Filter
	}
	if a.IsWriteIndex != nil {
		out["is_write_index"] = *a.IsWriteIndex
	}
	if a.Routing != "" {
		out["routing"] = a.Routing
	}
	if a.IndexRouting != "" {
		out["index_routing"] = a.IndexRouting
	}
	if a.SearchRouting != "" {
		out["search_routing"] = a.SearchRouting
	}
	return out
}

// Index is one OpenSearch index. It is shared between clusters by
// reference counting; a cluster copies it before the first write when it is
// shared (copy-on-write).
type Index struct {
	refs     atomic.Int32
	Name     string
	UUID     string
	Created  time.Time
	Settings M // nested settings, always {"index": {...}}
	Mapping  *Mapping
	Aliases  map[string]*Alias
	docs     map[string]*Doc
	children map[string][]string // bleve ids of the nested objects of each document
	seqNo    int64
	analysis *analysisSet
	bleve    bleve.Index
	warn     func(string)
	closed   bool
}

func newIndex(name string, settings M, mapping *Mapping, now time.Time, warn func(string)) (*Index, error) {
	as, err := buildAnalysis(settings, warn)
	if err != nil {
		return nil, err
	}
	// scorch with an empty path is a pure in-memory index (no persister);
	// bleve.NewMemOnly would use the much slower upsidedown/gtreap store.
	bi, err := bleve.NewUsing("", as.bmap, scorch.Name, scorch.Name, nil)
	if err != nil {
		return nil, err
	}
	ix := &Index{
		Name:     name,
		UUID:     newUUID(),
		Created:  now,
		Settings: settings,
		Mapping:  mapping,
		Aliases:  map[string]*Alias{},
		docs:     map[string]*Doc{},
		children: map[string][]string{},
		seqNo:    -1,
		analysis: as,
		bleve:    bi,
		warn:     warn,
	}
	ix.refs.Store(1)
	return ix, nil
}

// copyIndex creates an independent copy with its own bleve index.
func (ix *Index) copyIndex() (*Index, error) {
	n, err := newIndex(ix.Name, cloneDeep(ix.Settings).(M), ix.Mapping.clone(), ix.Created, ix.warn)
	if err != nil {
		return nil, err
	}
	n.UUID = ix.UUID
	for k, v := range ix.Aliases {
		a := *v
		n.Aliases[k] = &a
	}
	n.seqNo = ix.seqNo
	for id, d := range ix.docs {
		n.docs[id] = d
	}
	if err := n.rebuild(); err != nil {
		return nil, err
	}
	return n, nil
}

// rebuild re-indexes every stored document into bleve (after a copy or a
// mapping change that moved fields between nested levels).
func (ix *Index) rebuild() error {
	batch := ix.bleve.NewBatch()
	count := 0
	for id, d := range ix.docs {
		for _, cid := range ix.children[id] {
			batch.Delete(cid)
		}
		bds, err := ix.buildDocument(d, false)
		if err != nil {
			return err
		}
		if err := ix.addDocuments(batch, id, bds); err != nil {
			return err
		}
		count++
		if count%1000 == 0 {
			if err := ix.bleve.Batch(batch); err != nil {
				return err
			}
			batch = ix.bleve.NewBatch()
		}
	}
	return ix.bleve.Batch(batch)
}

// addDocuments queues the bleve documents of one stored document (the root
// first, then its nested objects) and records the nested ids.
func (ix *Index) addDocuments(batch *bleve.Batch, id string, bds []*document.Document) error {
	for _, bd := range bds {
		if err := batch.IndexAdvanced(bd); err != nil {
			return err
		}
	}
	if len(bds) > 1 {
		ids := make([]string, 0, len(bds)-1)
		for _, bd := range bds[1:] {
			ids = append(ids, bd.ID())
		}
		ix.children[id] = ids
	} else {
		delete(ix.children, id)
	}
	return nil
}

func (ix *Index) release() {
	if ix.refs.Add(-1) == 0 {
		ix.closed = true
		_ = ix.bleve.Close()
	}
}

// DocCount returns the number of stored documents.
func (ix *Index) DocCount() int { return len(ix.docs) }

func (ix *Index) sortedIDs() []string {
	ids := make([]string, 0, len(ix.docs))
	for id := range ix.docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var uuidCounter atomic.Int64

func newUUID() string {
	n := uuidCounter.Add(1)
	return fmt.Sprintf("osmem%016x%06x", time.Now().UnixNano(), n)[:22]
}

// document parsing -----------------------------------------------------

// parseSource parses a source document.
func parseSource(raw []byte) (M, []byte, error) {
	var v any
	if err := decodeJSON(raw, &v); err != nil {
		return nil, nil, errMapperParsing("failed to parse: %s", err.Error())
	}
	m, ok := v.(M)
	if !ok {
		return nil, nil, errMapperParsing("failed to parse, document is not an object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, nil, errMapperParsing("failed to parse: %s", err.Error())
	}
	return expandDots(m), buf.Bytes(), nil
}

// expandDots turns {"a.b": 1} into {"a": {"b": 1}} recursively.
func expandDots(m M) M {
	out := make(M, len(m))
	for k, v := range m {
		if sub, ok := v.(M); ok {
			v = expandDots(sub)
		} else if arr, ok := v.([]any); ok {
			v = expandDotsList(arr)
		}
		if strings.Contains(k, ".") && !strings.HasPrefix(k, ".") && !strings.HasSuffix(k, ".") {
			parts := strings.Split(k, ".")
			cur := out
			for i, p := range parts {
				if i == len(parts)-1 {
					cur[p] = mergeValue(cur[p], v)
					break
				}
				next, ok := cur[p].(M)
				if !ok {
					next = M{}
					cur[p] = next
				}
				cur = next
			}
			continue
		}
		out[k] = mergeValue(out[k], v)
	}
	return out
}

func expandDotsList(arr []any) []any {
	out := make([]any, len(arr))
	for i, e := range arr {
		switch t := e.(type) {
		case M:
			out[i] = expandDots(t)
		case []any:
			out[i] = expandDotsList(t)
		default:
			out[i] = e
		}
	}
	return out
}

func mergeValue(existing, v any) any {
	if existing == nil {
		return v
	}
	em, ok1 := existing.(M)
	vm, ok2 := v.(M)
	if ok1 && ok2 {
		for k, val := range vm {
			em[k] = mergeValue(em[k], val)
		}
		return em
	}
	return v
}

// cloneDeep copies a JSON tree.
func cloneDeep(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, e := range t {
			out[k] = cloneDeep(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneDeep(e)
		}
		return out
	}
	return v
}

// document building ----------------------------------------------------

type pendingField struct {
	target map[string]*Field
	name   string
	path   string
	field  *Field
}

type docBuilder struct {
	ix            *Index
	id            string // bleve id of the document being built
	level         string // nested path of the document ("" for the root)
	doc           *document.Document
	exists        map[string]bool
	pending       []pendingField
	copyTo        map[string][]any
	infer         bool
	nestedCount   map[string]int       // objects seen per nested path below this level
	nestedTotal   int                  // nested objects seen in this source document
	children      []*document.Document // nested documents (root builder only)
	root          *docBuilder          // root builder (nil for the root itself)
	mappingBefore *Mapping             // lazily captured if copy_to mutates the mapping
}

// buildDocument converts a stored document into bleve documents following
// the index mapping: the root document first, then one document per
// nested object. When infer is true, unmapped fields are added to the
// mapping (dynamic mapping).
func (ix *Index) buildDocument(d *Doc, infer bool) (_ []*document.Document, err error) {
	b := &docBuilder{ix: ix, id: d.ID, doc: document.NewDocument(d.ID), exists: map[string]bool{}, infer: infer}
	defer func() {
		if err != nil && b.mappingBefore != nil {
			ix.Mapping = b.mappingBefore
		}
	}()
	if getBool(ix.Mapping.Extra, "enabled", true) {
		if err := b.walkObject("", d.Src, ix.Mapping.Properties, ix.Mapping.Dynamic, nil); err != nil {
			return nil, err
		}
	}
	if err := b.finish(); err != nil {
		return nil, err
	}
	if infer && (len(b.pending) > 0 || b.mappingBefore != nil) {
		candidate := ix.Mapping.clone()
		applyPendingMappingFields(candidate, b.pending)
		if err := validateMappingLimits(candidate, ix.Settings); err != nil {
			return nil, err
		}
	}
	b.doc.AddField(document.NewTextFieldCustom("_id", nil, []byte(d.ID), index.IndexField, ix.keywordAnalyzer()))
	b.doc.AddField(document.NewTextFieldCustom(fieldRoot, nil, []byte("1"), index.IndexField, ix.keywordAnalyzer()))
	b.doc.AddField(document.NewCompositeFieldWithIndexingOptions("_all", true, nil, []string{"_exists_", "_id"}, index.IndexField|index.IncludeTermVectors))
	for _, p := range b.pending {
		p.target[p.name] = p.field
	}
	return append([]*document.Document{b.doc}, b.children...), nil
}

// finish adds the copy_to targets and the _exists_ markers of one document.
func (b *docBuilder) finish() error {
	ix := b.ix
	if b.infer && len(b.copyTo) > 0 {
		root := b
		if root.root != nil {
			root = root.root
		}
		for target := range b.copyTo {
			if _, _, ok := ix.Mapping.resolve(target); !ok {
				if root.mappingBefore == nil {
					root.mappingBefore = ix.Mapping.clone()
				}
				break
			}
		}
	}
	for target, vals := range b.copyTo {
		f, _, ok := ix.Mapping.resolve(target)
		if !ok {
			if !b.infer {
				continue
			}
			var err error
			f, err = ix.Mapping.inferTreeForPath(target, vals[0])
			if err != nil {
				return err
			}
			if f == nil {
				continue
			}
			parts := strings.Split(target, ".")
			fields := ix.Mapping.Properties
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
		}
		for i, v := range vals {
			if err := b.addLeaf(target, f, v, []uint64{uint64(i)}); err != nil {
				return err
			}
		}
	}
	for path := range b.exists {
		b.doc.AddField(document.NewTextFieldCustom("_exists_", nil, []byte(path), index.IndexField, ix.keywordAnalyzer()))
	}
	return nil
}

func mappingFieldPaths(fields map[string]*Field, prefix string, paths map[string]bool) {
	for name, field := range fields {
		path := prefix + name
		paths[path] = true
		mappingFieldPaths(field.Properties, path+".", paths)
		mappingFieldPaths(field.Fields, path+".", paths)
	}
}

func countMappingFieldPaths(fields map[string]*Field) int {
	paths := map[string]bool{}
	mappingFieldPaths(fields, "", paths)
	return len(paths)
}

// validateMappingLimits applies the index-level mapping limits that OpenSearch
// enforces when a mapping is created or extended.
func validateMappingLimits(mapping *Mapping, settings M) error {
	limits := getMap(getMap(settings, "index"), "mapping")
	totalFieldsLimit := getInt(getMap(limits, "total_fields"), "limit", 1000)
	if count := countMappingFieldPaths(mapping.Properties); count > totalFieldsLimit {
		return errMapperParsing("Limit of total fields [%d] has been exceeded", totalFieldsLimit)
	}
	depthLimit := getInt(getMap(limits, "depth"), "limit", 20)
	if path, depth := deepestMappingPath(mapping.Properties); depth > depthLimit {
		return errMapperParsing("Limit of mapping depth [%d] has been exceeded due to the field [%s]", depthLimit, path)
	}
	nestedLimit := getInt(getMap(limits, "nested_fields"), "limit", 50)
	if nested := countNestedMappingFields(mapping.Properties); nested > nestedLimit {
		return errMapperParsing("Limit of nested fields [%d] has been exceeded", nestedLimit)
	}
	return nil
}

// deepestMappingPath counts root-level fields at depth 1 and increments depth
// only when descending through an object mapping. Multi-fields are not object
// nesting and therefore do not increase mapping depth.
func deepestMappingPath(fields map[string]*Field) (string, int) {
	var deepestPath string
	var deepest int
	var walk func(map[string]*Field, string, int)
	walk = func(fields map[string]*Field, prefix string, depth int) {
		for name, field := range fields {
			path := prefix + name
			if depth > deepest {
				deepestPath, deepest = path, depth
			}
			if len(field.Properties) > 0 {
				walk(field.Properties, path+".", depth+1)
			}
		}
	}
	walk(fields, "", 1)
	return deepestPath, deepest
}

func countNestedMappingFields(fields map[string]*Field) int {
	count := 0
	for _, field := range fields {
		if field.Type == TypeNested {
			count++
		}
		count += countNestedMappingFields(field.Properties)
	}
	return count
}

// applyPendingMappingFields builds the prospective mapping for a document's
// inferred fields without mutating the live mapping before limit validation.
func applyPendingMappingFields(mapping *Mapping, pending []pendingField) {
	for _, item := range pending {
		parts := strings.Split(item.path, ".")
		fields := mapping.Properties
		for _, part := range parts[:len(parts)-1] {
			field := fields[part]
			if field == nil {
				field = &Field{Type: TypeObject, Index: true, Enabled: true, Properties: map[string]*Field{}, inferred: true}
				fields[part] = field
			}
			if field.Properties == nil {
				field.Properties = map[string]*Field{}
			}
			fields = field.Properties
		}
		if len(parts) > 0 {
			fields[parts[len(parts)-1]] = item.field.clone()
		}
	}
}

// buildNested indexes the objects of a nested field as documents of their
// own, numbered in index order below the current level.
func (b *docBuilder) buildNested(full string, f *Field, val any, dynamic string) error {
	root := b.root
	if root == nil {
		root = b
	}
	limit := getInt(getMap(getMap(getMap(root.ix.Settings, "index"), "mapping"), "nested_objects"), "limit", 10000)
	if b.nestedCount == nil {
		b.nestedCount = map[string]int{}
	}
	for _, e := range flattenValues(val) {
		m, ok := e.(M)
		if !ok {
			return errMapperParsing("object mapping for [%s] tried to parse field [%s] as object, but found a concrete value", full, full)
		}
		root.nestedTotal++
		if root.nestedTotal > limit {
			return errMapperParsing("nested object limit [%d] exceeded for field [%s]", limit, full)
		}
		off := b.nestedCount[full]
		b.nestedCount[full]++
		cb := &docBuilder{ix: b.ix, id: nestedID(b.id, full, off), level: full, exists: map[string]bool{}, infer: b.infer, root: root}
		cb.doc = document.NewDocument(cb.id)
		if err := cb.walkObject(full+".", m, f.Properties, dynamic, nil); err != nil {
			return err
		}
		if err := cb.finish(); err != nil {
			return err
		}
		cb.doc.AddField(document.NewTextFieldCustom(fieldNestedPath, nil, []byte(full), index.IndexField, b.ix.keywordAnalyzer()))
		cb.doc.AddField(document.NewCompositeFieldWithIndexingOptions("_all", true, nil, []string{"_exists_"}, index.IndexField|index.IncludeTermVectors))
		root.children = append(root.children, cb.doc)
		root.pending = append(root.pending, cb.pending...)
	}
	return nil
}

func (ix *Index) keywordAnalyzer() analysis.Analyzer {
	a, _ := ix.analysis.analyzerNamed("keyword")
	return a
}

func (b *docBuilder) walkObject(prefix string, obj M, fields map[string]*Field, dynamic string, arrayPos []uint64) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := obj[key]
		full := prefix + key
		f, ok := fields[key]
		if val == nil && (!ok || f.NullValue == nil) {
			continue
		}
		if !ok {
			switch dynamic {
			case "false":
				continue
			case "strict":
				return errStrictDynamic(full)
			}
			if !b.infer {
				continue
			}
			var err error
			f, err = b.ix.Mapping.inferTreeForPath(full, val)
			if err != nil {
				return err
			}
			if f == nil {
				continue
			}
			b.pending = append(b.pending, pendingField{target: fields, name: key, path: full, field: f})
		}
		if err := b.walkField(full, f, val, dynamic, arrayPos); err != nil {
			return err
		}
	}
	return nil
}

func (b *docBuilder) walkField(full string, f *Field, val any, dynamic string, arrayPos []uint64) error {
	switch f.Type {
	case TypeObject, TypeNested:
		if !f.Enabled {
			b.exists[full] = true
			return nil
		}
		d := dynamic
		if f.Dynamic != "" {
			d = f.Dynamic
		}
		if f.Properties == nil {
			f.Properties = map[string]*Field{}
		}
		if f.Type == TypeNested {
			// nested objects live in documents of their own; the parent
			// keeps no trace of them, as on OpenSearch
			return b.buildNested(full, f, val, d)
		}
		switch t := val.(type) {
		case M:
			b.exists[full] = true
			return b.walkObject(full+".", t, f.Properties, d, arrayPos)
		case []any:
			for i, e := range t {
				if e == nil {
					continue
				}
				m, ok := e.(M)
				if !ok {
					// OpenSearch rejects concrete values for object fields
					return errMapperParsing("object mapping for [%s] tried to parse field [%s] as object, but found a concrete value", full, full)
				}
				b.exists[full] = true
				if err := b.walkObject(full+".", m, f.Properties, d, append(append([]uint64(nil), arrayPos...), uint64(i))); err != nil {
					return err
				}
			}
			return nil
		default:
			return errMapperParsing("object mapping for [%s] tried to parse field [%s] as object, but found a concrete value", full, full)
		}
	case TypeAlias:
		return nil
	}
	vals := leafValues(f, val)
	if len(vals) == 0 {
		if f.NullValue != nil {
			vals = []any{f.NullValue}
		} else {
			return nil
		}
	}
	for i, v := range vals {
		if v == nil {
			if f.NullValue == nil {
				continue
			}
			v = f.NullValue
		}
		pos := arrayPos
		if len(vals) > 1 || len(arrayPos) > 0 {
			pos = append(append([]uint64(nil), arrayPos...), uint64(i))
		}
		if err := b.addLeaf(full, f, v, pos); err != nil {
			return err
		}
		for sn, sf := range f.Fields {
			if err := b.addLeaf(full+"."+sn, sf, v, pos); err != nil {
				return err
			}
		}
		for _, target := range f.CopyTo {
			if b.copyTo == nil {
				b.copyTo = map[string][]any{}
			}
			b.copyTo[target] = append(b.copyTo[target], v)
		}
	}
	// mark existence of the field and its parents
	parts := strings.Split(full, ".")
	for i := range parts {
		b.exists[strings.Join(parts[:i+1], ".")] = true
	}
	return nil
}

// leafValues lists the values of a leaf field. geo_point arrays
// ([lon, lat]) are kept as one value.
func leafValues(f *Field, v any) []any {
	if f != nil && f.Type == TypeGeoPoint {
		if arr, ok := v.([]any); ok {
			if len(arr) == 2 {
				if _, ok := toFloat(arr[0]); ok {
					return []any{v}
				}
			}
			var out []any
			for _, e := range arr {
				out = append(out, leafValues(f, e)...)
			}
			return out
		}
		if v == nil {
			return nil
		}
		return []any{v}
	}
	return flattenValues(v)
}

// flattenValues turns a value into the list of its scalar values.
func flattenValues(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []any
		for _, e := range t {
			out = append(out, flattenValues(e)...)
		}
		return out
	default:
		return []any{v}
	}
}

func (b *docBuilder) addLeaf(name string, f *Field, v any, arrayPos []uint64) error {
	ix := b.ix
	if !f.Index {
		return nil
	}
	opts := index.IndexField | index.IncludeTermVectors
	switch f.Type {
	case TypeText, TypeSearchAsYouType:
		s, err := stringValue(name, f, v)
		if err != nil {
			return err
		}
		an, err := ix.analysis.analyzerNamed(f.Analyzer)
		if err != nil {
			return err
		}
		b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(s), opts, an))
	case TypeKeyword, TypeConstantKeyword, TypeWildcard, TypeIP, TypeVersion:
		s, err := stringValue(name, f, v)
		if err != nil {
			return err
		}
		if (f.IgnoreAbove > 0 || f.ignoreAboveSet && f.IgnoreAbove == 0) && utf8.RuneCountInString(s) > f.IgnoreAbove {
			return nil
		}
		an, err := ix.analysis.normalizerNamed(f.Normalizer)
		if err != nil {
			return err
		}
		b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(s), opts, an))
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat, TypeScaledFloat, TypeUnsignedLong, TypeTokenCount:
		if _, isObj := v.(M); isObj {
			return errMapperParsing("failed to parse field [%s] of type [%s] in document", name, f.Type)
		}
		if !ix.coerceEnabled(f) {
			if _, isString := v.(string); isString {
				return errMapperParsing("failed to parse field [%s] of type [%s] in document with id '%s'. Preview of field's value: '%v'", name, f.Type, b.doc.ID(), v)
			}
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			return nil
		}
		num, ok := toFloat(v)
		if !ok {
			return errMapperParsing("failed to parse field [%s] of type [%s] in document with id '%s'. Preview of field's value: '%v'", name, f.Type, b.doc.ID(), v)
		}
		if f.isIntegral() && num != math.Trunc(num) {
			if !ix.coerceEnabled(f) {
				return errMapperParsing("failed to parse field [%s] of type [%s] in document with id '%s'. Preview of field's value: '%v'", name, f.Type, b.doc.ID(), v)
			}
			num = math.Trunc(num)
		}
		b.doc.AddField(document.NewNumericFieldWithIndexingOptions(name, arrayPos, num, index.IndexField))
		if f.isIntegral() {
			if exact, ok := integralString(v); ok {
				b.doc.AddField(document.NewTextFieldCustom(exactNumericField(name), arrayPos, []byte(exact), index.IndexField, ix.keywordAnalyzer()))
			}
		}
	case TypeBoolean:
		bv, ok := boolValue(v)
		if !ok {
			return errMapperParsing("failed to parse field [%s] of type [boolean] in document with id '%s'. Preview of field's value: '%v'", name, b.doc.ID(), v)
		}
		b.doc.AddField(document.NewBooleanFieldWithIndexingOptions(name, arrayPos, bv, index.IndexField))
	case TypeDate, TypeDateNanos:
		df := f.Format
		if df == nil {
			df = ParseDateFormat(DefaultDateFormat)
		}
		t, err := df.Parse(v)
		if err != nil {
			return errMapperParsing("failed to parse field [%s] of type [date] in document with id '%s'. Preview of field's value: '%v'", name, b.doc.ID(), v)
		}
		fld, err := document.NewDateTimeFieldWithIndexingOptions(name, arrayPos, t, "", index.IndexField)
		if err != nil {
			return errMapperParsing("failed to parse field [%s] of type [date]: %v", name, err)
		}
		b.doc.AddField(fld)
	case TypeGeoPoint:
		lat, lon, ok := geoPointValue(v)
		if !ok {
			return errMapperParsing("failed to parse field [%s] of type [geo_point]", name)
		}
		b.doc.AddField(document.NewGeoPointFieldWithIndexingOptions(name, arrayPos, lon, lat, index.IndexField))
	default:
		// binary, knn_vector, completion, ranges, ...: stored only
	}
	return nil
}

func (ix *Index) coerceEnabled(f *Field) bool {
	if f != nil {
		if _, ok := f.Extra["coerce"]; ok {
			return getBool(f.Extra, "coerce", true)
		}
	}
	idx := getMap(ix.Settings, "index")
	return getBool(getMap(idx, "mapping"), "coerce", true)
}

func stringValue(name string, f *Field, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(t), nil
	case M:
		return "", errMapperParsing("failed to parse field [%s] of type [%s] in document. Can't get text on a START_OBJECT", name, f.Type)
	}
	return fmt.Sprint(v), nil
}

func boolValue(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch t {
		case "true":
			return true, true
		case "false", "":
			return false, true
		}
	}
	return false, false
}

// geoPointValue parses the geo_point representations: {"lat":..,"lon":..},
// "lat,lon", [lon, lat], "POINT (lon lat)".
func geoPointValue(v any) (lat, lon float64, ok bool) {
	switch t := v.(type) {
	case M:
		la, ok1 := toFloat(t["lat"])
		lo, ok2 := toFloat(t["lon"])
		return la, lo, ok1 && ok2
	case []any:
		if len(t) != 2 {
			return 0, 0, false
		}
		lo, ok1 := toFloat(t[0])
		la, ok2 := toFloat(t[1])
		return la, lo, ok1 && ok2
	case string:
		s := strings.TrimSpace(t)
		if strings.HasPrefix(strings.ToUpper(s), "POINT") {
			s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToUpper(s), "POINT"), " ("), ")"))
			s = strings.Trim(s, "()")
			parts := strings.Fields(s)
			if len(parts) != 2 {
				return 0, 0, false
			}
			lo, err1 := strconv.ParseFloat(parts[0], 64)
			la, err2 := strconv.ParseFloat(parts[1], 64)
			return la, lo, err1 == nil && err2 == nil
		}
		parts := strings.Split(s, ",")
		if len(parts) != 2 {
			return 0, 0, false
		}
		la, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lo, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		return la, lo, err1 == nil && err2 == nil
	}
	return 0, 0, false
}

// inferTree infers a mapping for a value including nested objects.
func (m *Mapping) inferTree(v any) *Field {
	switch t := v.(type) {
	case []any:
		var f *Field
		for _, e := range t {
			if e == nil {
				continue
			}
			ef := m.inferTree(e)
			if ef == nil {
				continue
			}
			if f == nil {
				f = ef
				continue
			}
			if f.Type == TypeObject && ef.Type == TypeObject {
				for k, sub := range ef.Properties {
					if _, ok := f.Properties[k]; !ok {
						f.Properties[k] = sub
					}
				}
			}
		}
		return f
	case M:
		f := m.inferField(t)
		for k, e := range t {
			if e == nil {
				continue
			}
			if sub := m.inferTree(e); sub != nil {
				f.Properties[k] = sub
			}
		}
		return f
	default:
		return m.inferField(v)
	}
}

// value extraction for sorting and aggregations -----------------------

// fieldValues returns the values of a field in a document, converted to the
// mapped type: string for keyword/text, float64 for numbers, bool, and
// time.Time for dates. path may address a multi-field (title.keyword).
func (ix *Index) fieldValues(d *Doc, path string) []any {
	return ix.fieldValuesAt(d, path, false)
}

func (ix *Index) storedFieldValues(d *Doc, fields []string) M {
	out := M{}
	for _, name := range fields {
		if name == "" || name == "_none_" {
			continue
		}
		for _, path := range ix.Mapping.leafFields(name) {
			f, _, ok := ix.Mapping.resolve(path)
			if !ok || !getBool(f.Extra, "store", false) {
				continue
			}
			if vals := ix.fieldValues(d, path); len(vals) > 0 {
				out[path] = vals
			}
		}
	}
	return out
}

// fieldValuesAt is fieldValues with anyLevel reading the values of nested
// fields straight from the source whatever the document's level (the
// fields option of a search does that on OpenSearch; doc values, sorts and
// aggregations do not).
func (ix *Index) fieldValuesAt(d *Doc, path string, anyLevel bool) []any {
	switch path {
	case "_id":
		return []any{d.ID}
	case "_index":
		return []any{ix.Name}
	case "_seq_no":
		return []any{float64(d.SeqNo)}
	case "_version":
		return []any{float64(d.Version)}
	}
	f, base, ok := ix.Mapping.resolve(path)
	if !ok {
		return nil
	}
	// fields of nested objects belong to the nested documents, not to the
	// root (and root fields are not visible from a nested object)
	if !anyLevel && ix.Mapping.nestedAncestor(base) != d.level() {
		return nil
	}
	raw := leafValues(f, lookupPath(d.Src, base))
	if len(raw) == 0 {
		return nil
	}
	out := make([]any, 0, len(raw))
	for _, v := range raw {
		if v == nil {
			if f.NullValue != nil {
				v = f.NullValue
			} else {
				continue
			}
		}
		if cv, ok := convertValue(f, v); ok {
			out = append(out, cv)
		}
	}
	return out
}

func convertValue(f *Field, v any) (any, bool) {
	switch {
	case f.isNumeric():
		n, ok := toFloat(v)
		return n, ok
	case f.isDate():
		df := f.Format
		if df == nil {
			df = ParseDateFormat(DefaultDateFormat)
		}
		t, err := df.Parse(v)
		return t, err == nil
	case f.Type == TypeBoolean:
		b, ok := boolValue(v)
		return b, ok
	case f.Type == TypeGeoPoint:
		lat, lon, ok := geoPointValue(v)
		return [2]float64{lat, lon}, ok
	case f.Type == TypeObject, f.Type == TypeNested:
		return nil, false
	default:
		s, err := stringValue("", f, v)
		if err != nil {
			return nil, false
		}
		if f.IgnoreAbove > 0 && utf8.RuneCountInString(s) > f.IgnoreAbove {
			return nil, false
		}
		return s, true
	}
}

// lookupPath extracts a dotted path from a source tree, descending into
// arrays of objects.
func lookupPath(v any, path string) any {
	if path == "" {
		return v
	}
	parts := strings.Split(path, ".")
	return lookupParts(v, parts)
}

func lookupParts(v any, parts []string) any {
	if len(parts) == 0 {
		return v
	}
	switch t := v.(type) {
	case M:
		next, ok := t[parts[0]]
		if !ok {
			return nil
		}
		return lookupParts(next, parts[1:])
	case []any:
		var out []any
		for _, e := range t {
			r := lookupParts(e, parts)
			if r == nil {
				continue
			}
			if arr, ok := r.([]any); ok {
				out = append(out, arr...)
			} else {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}
