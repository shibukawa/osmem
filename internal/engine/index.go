package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
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
	// Ignored lists the fields whose malformed values were ignored
	// (ignore_malformed), the _ignored metadata field
	Ignored []string
	// nested objects are represented by synthetic documents (see nested.go)
	nested []nestedLevel // identity of the object; nil for root documents
	obj    M             // the nested object itself
	root   *Doc          // the root document of a nested object
	// dateCache holds the parsed values of the document's own date and
	// date_nanos fields, keyed by their full mapping path, so sorting reads
	// the already-parsed value instead of re-parsing it from Src on every
	// search. Set once building the bleve document has fully succeeded (see
	// buildDocumentFrom and docBuilder.dateCache); nested fields are
	// excluded (see addLeaf) since a value here would mix the values of
	// every nested object. Left nil on synthetic nested documents (root !=
	// nil), which still sort from Src.
	dateCache map[string][]time.Time
	// routing is the custom routing the document was indexed with ("" for
	// the default routing by id); see docRouting/setDocRouting
	routing string
}

// Alias is an index alias definition.
type Alias struct {
	Filter        M
	IsWriteIndex  *bool
	Routing       string
	IndexRouting  string
	SearchRouting string
	IsHidden      *bool
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
	if a.IsHidden != nil {
		out["is_hidden"] = *a.IsHidden
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
	runs     []*segmentRun          // one per bleve segment, oldest first (see segments.go)
	runOf    map[string]*segmentRun // run holding the indexed version of each document
	// mappingGen counts mapping updates; runs indexed before the last one are
	// not merged (see segments.go)
	mappingGen int
	warn       func(string)
	closed     bool
	// stateClosed is the CLOSE index state (POST /{index}/_close); closed
	// above only means the bleve index was released.
	stateClosed bool
	// reopenRebuild marks analysis settings changed while the index was
	// closed: opening it rebuilds the analyzers.
	reopenRebuild bool
	// copies counts the copies made of this index (copyIndex); the
	// tombstone history uses it to tell whether a snapshot may still be
	// inherited by a copy (see tombstones.go)
	copies atomic.Int64
	// ordinals caches the shard document ordinals of the current documents
	// (see shardDocOrdinals in searchexec.go)
	ordinals atomic.Pointer[shardOrdinalCache]
}

func newIndex(name string, settings M, mapping *Mapping, now time.Time, warn func(string)) (*Index, error) {
	as, err := buildAnalysis(settings, warn)
	if err != nil {
		return nil, err
	}
	if err := mapping.validateAnalysis(as, settings); err != nil {
		return nil, wrapFailedMapping(asError(err))
	}
	mapping.analysis, mapping.settings = as, settings
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
		runOf:    map[string]*segmentRun{},
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
	// the copy may later inherit the tombstones as of now: freeze the
	// current snapshot (see tombstoneState.mutable)
	ix.copies.Add(1)
	n, err := newIndex(ix.Name, cloneDeep(ix.Settings).(M), ix.Mapping.clone(), ix.Created, ix.warn)
	if err != nil {
		return nil, err
	}
	n.UUID = ix.UUID
	n.stateClosed = ix.stateClosed
	n.reopenRebuild = ix.reopenRebuild
	for k, v := range ix.Aliases {
		a := *v
		n.Aliases[k] = &a
	}
	n.seqNo = ix.seqNo
	for id, d := range ix.docs {
		n.docs[id] = d
	}
	if err := n.rebuild(); err != nil {
		// drop the bleve index of the abandoned copy (scorch runs
		// background goroutines until closed)
		n.release()
		return nil, err
	}
	return n, nil
}

// rebuild re-indexes every stored document into bleve (after a copy or a
// mapping change that moved fields between nested levels). It rebuilds each
// document onto a copy of its Doc, not d itself: copyIndex shares its docs
// with the index it copied, which a concurrent search on that other index
// may still be reading, and buildDocument mutates derived fields of the Doc
// it is given (Ignored, dateCache), so mutating the shared d in place would
// race with that read.
func (ix *Index) rebuild() error {
	batch := ix.newBatch()
	count := 0
	for id, d := range ix.docs {
		nd := *d
		bds, err := ix.buildDocument(&nd, false)
		if err != nil {
			return err
		}
		ix.docs[id] = &nd
		if err := ix.addDocuments(batch, id, bds); err != nil {
			return err
		}
		count++
		if count%reindexBatchDocs == 0 {
			if err := ix.commit(batch); err != nil {
				return err
			}
			batch = ix.newBatch()
		}
	}
	return ix.commit(batch)
}

// addDocuments queues the bleve documents of one stored document (the root
// first, then its nested objects) in place of its previous version and
// records the nested ids.
func (ix *Index) addDocuments(batch *docBatch, id string, bds []*document.Document) error {
	// nested objects of the previous version that no longer exist must go;
	// the ones that still exist are overwritten by the new documents
	for _, cid := range ix.children[id] {
		batch.Delete(cid)
	}
	for _, bd := range bds {
		if err := batch.IndexAdvanced(bd); err != nil {
			return err
		}
	}
	batch.roots = append(batch.roots, rootOp{id: id})
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

var uuidCounter atomic.Int64

func newUUID() string {
	n := uuidCounter.Add(1)
	return fmt.Sprintf("osmem%016x%06x", time.Now().UnixNano(), n)[:22]
}

// document parsing -----------------------------------------------------

// expandDots turns {"a.b": 1} into {"a": {"b": 1}} recursively. Trailing
// dots are dropped like String.split does; keys that cannot be split into
// field names (".a", "a..b", ".") are kept for the parser to reject.
func expandDots(m M) M {
	out := make(M, len(m))
	for k, v := range m {
		if sub, ok := v.(M); ok {
			v = expandDots(sub)
		} else if arr, ok := v.([]any); ok {
			v = expandDotsList(arr)
		}
		if strings.Contains(k, ".") {
			parts := splitJavaPath(k)
			valid := len(parts) > 0
			for _, p := range parts {
				if strings.TrimSpace(p) == "" {
					valid = false
				}
			}
			if valid {
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
	src           *Doc   // the stored (root) document
	id            string // bleve id of the document being built
	level         string // nested path of the document ("" for the root)
	doc           *document.Document
	exists        map[string]bool
	pending       []pendingField
	copyTo        map[string][]any
	infer         bool
	nestedCount   map[string]int         // objects seen per nested path below this level
	nestedTotal   int                    // nested objects seen in this source document
	children      []*document.Document   // nested documents (root builder only)
	root          *docBuilder            // root builder (nil for the root itself)
	mappingBefore *Mapping               // lazily captured if copy_to mutates the mapping
	occ           map[string]int         // value tokens seen per source path (root builder)
	ignored       map[string]bool        // fields with ignored malformed values (root builder)
	dateCache     map[string][]time.Time // date/date_nanos values of the root's own fields, staged until the build succeeds (root builder)
	tree          *rawNode               // parsed request body or source (root builder, lazily)
	seen          map[string]bool        // single valued features indexed in this document
	parent        *docBuilder            // builder of the enclosing document (nested objects)
	// shadow builders add the fields of nested objects to an enclosing
	// document (include_in_parent, include_in_root)
	shadow bool
	// body is the request body the source was parsed from, where OpenSearch
	// locates parse errors (nil: the stored source)
	body []byte
	// limits caches the index settings read per value while indexing (root
	// builder, see settingsLimits)
	limits *docLimits
}

// docLimits are the index.mapping.* settings consulted while indexing a
// document, read once per document rather than once per value.
type docLimits struct {
	depth  int  // index.mapping.depth.limit
	nested int  // index.mapping.nested_objects.limit
	coerce bool // index.mapping.coerce
}

// settingsLimits returns the index settings limits, reading them on the
// first call for the document.
func (b *docBuilder) settingsLimits() *docLimits {
	root := b.rootBuilder()
	if root.limits == nil {
		mapping := getMap(getMap(root.ix.Settings, "index"), "mapping")
		root.limits = &docLimits{
			depth:  getInt(getMap(mapping, "depth"), "limit", 20),
			nested: getInt(getMap(mapping, "nested_objects"), "limit", 10000),
			coerce: getBool(mapping, "coerce", true),
		}
	}
	return root.limits
}

// coerceEnabled is Index.coerceEnabled with the settings default cached
// for the document.
func (b *docBuilder) coerceEnabled(f *Field) bool {
	if f != nil {
		if _, ok := f.Extra["coerce"]; ok {
			return getBool(f.Extra, "coerce", true)
		}
	}
	return b.settingsLimits().coerce
}

// defaultDateFormat is the parsed DefaultDateFormat, for date fields
// without a format of their own.
var defaultDateFormat = ParseDateFormat(DefaultDateFormat)

// locationSource returns the bytes the locations of parse errors refer to.
func (b *docBuilder) locationSource() []byte {
	if b.body != nil {
		return b.body
	}
	return b.src.Raw
}

func (b *docBuilder) rootBuilder() *docBuilder {
	if b.root != nil {
		return b.root
	}
	return b
}

// buildDocument converts a stored document into bleve documents following
// the index mapping: the root document first, then one document per
// nested object. When infer is true, unmapped fields are added to the
// mapping (dynamic mapping).
func (ix *Index) buildDocument(d *Doc, infer bool) (_ []*document.Document, err error) {
	return ix.buildDocumentFrom(d, nil, infer)
}

// buildDocumentFrom is buildDocument for a source parsed from body, the
// request bytes parse errors are located in (nil: the stored source).
func (ix *Index) buildDocumentFrom(d *Doc, body []byte, infer bool) (_ []*document.Document, err error) {
	b := &docBuilder{ix: ix, src: d, id: d.ID, doc: document.NewDocument(d.ID), exists: map[string]bool{}, infer: infer,
		occ: map[string]int{}, ignored: map[string]bool{}, body: body}
	defer func() {
		if err != nil && b.mappingBefore != nil {
			ix.Mapping = b.mappingBefore
		}
	}()
	if getBool(ix.Mapping.Extra, "enabled", true) {
		if err := b.walkObject("", d.Src, ix.Mapping.Properties, ix.Mapping.Dynamic, ix.Mapping.Dynamic, nil); err != nil {
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
	d.dateCache = b.dateCache
	d.Ignored = nil
	for _, name := range sortedKeys(b.ignored) {
		d.Ignored = append(d.Ignored, name)
		b.doc.AddField(document.NewTextFieldCustom("_ignored", nil, []byte(name), index.IndexField, ix.keywordAnalyzer()))
	}
	if len(b.ignored) > 0 {
		b.doc.AddField(document.NewTextFieldCustom("_exists_", nil, []byte("_ignored"), index.IndexField, ix.keywordAnalyzer()))
	}
	b.doc.AddField(document.NewTextFieldCustom("_id", nil, []byte(d.ID), index.IndexField, ix.keywordAnalyzer()))
	b.doc.AddField(document.NewTextFieldCustom(fieldRoot, nil, []byte("1"), index.IndexField, ix.keywordAnalyzer()))
	b.doc.AddField(document.NewCompositeFieldWithIndexingOptions("_all", true, nil, []string{"_exists_", "_id"}, index.IndexField|index.IncludeTermVectors))
	for _, p := range b.pending {
		p.target[p.name] = p.field
	}
	return append([]*document.Document{b.doc}, b.children...), nil
}

// fieldAtPath finds the mapper of a path without following aliases.
func (m *Mapping) fieldAtPath(path string) *Field {
	parts := strings.Split(path, ".")
	fields := m.Properties
	var cur *Field
	for i, p := range parts {
		f, ok := fields[p]
		if !ok {
			if cur != nil && i == len(parts)-1 {
				return cur.Fields[p]
			}
			return nil
		}
		cur = f
		fields = f.Properties
	}
	return cur
}

func errFailedToParse(cause *Error) *Error {
	return &Error{Status: 400, Type: "mapper_parsing_exception", Reason: "failed to parse", Cause: cause}
}

// finish adds the copy_to targets and the _exists_ markers of one document.
func (b *docBuilder) finish() error {
	ix := b.ix
	if b.infer && len(b.copyTo) > 0 {
		root := b.rootBuilder()
		for target := range b.copyTo {
			if _, _, ok := ix.Mapping.resolve(target); !ok {
				if root.mappingBefore == nil {
					root.mappingBefore = ix.Mapping.clone()
				}
				break
			}
		}
	}
	targets := make([]string, 0, len(b.copyTo))
	for target := range b.copyTo {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		vals := b.copyTo[target]
		if af := ix.Mapping.fieldAtPath(target); af != nil && af.Type == TypeAlias {
			return errFailedToParse(errIllegalArgument("Cannot copy to a field alias [%s].", target))
		}
		f, _, ok := ix.Mapping.resolve(target)
		if !ok {
			if !b.infer {
				continue
			}
			var err error
			if f, err = b.dynamicCopyTarget(target, vals[0]); err != nil {
				return err
			}
			if f == nil {
				continue
			}
		}
		for i, v := range vals {
			pos := []uint64{uint64(i)}
			indexed, err := b.addLeaf(target, target, f, v, pos, i)
			if err != nil {
				return err
			}
			if indexed {
				b.markExists(target)
			}
			for _, sn := range sortedFieldNames(f.Fields) {
				subIndexed, err := b.addLeaf(target+"."+sn, target, f.Fields[sn], v, pos, i)
				if err != nil {
					return err
				}
				if subIndexed {
					b.exists[target+"."+sn] = true
				}
			}
		}
	}
	for path := range b.exists {
		b.doc.AddField(document.NewTextFieldCustom("_exists_", nil, []byte(path), index.IndexField, ix.keywordAnalyzer()))
	}
	return nil
}

func sortedFieldNames(fields map[string]*Field) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// markExists records that a field and its parent objects have a value.
func (b *docBuilder) markExists(path string) {
	b.exists[path] = true
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			b.exists[path[:i]] = true
		}
	}
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
	nestedLimit := getInt(getMap(limits, "nested_fields"), "limit", 50)
	if nested := countNestedMappingFields(mapping.Properties); nested > nestedLimit {
		return errIllegalArgument("Limit of nested fields [%d] has been exceeded", nestedLimit)
	}
	totalFieldsLimit := getInt(getMap(limits, "total_fields"), "limit", 1000)
	if count := countMappingFieldPaths(mapping.Properties); count > totalFieldsLimit {
		return errIllegalArgument("Limit of total fields [%d] has been exceeded", totalFieldsLimit)
	}
	depthLimit := getInt(getMap(limits, "depth"), "limit", 20)
	if path := deepObjectPath(mapping.Properties, depthLimit); path != "" {
		return errIllegalArgument("Limit of mapping depth [%d] has been exceeded due to object field [%s]", depthLimit, path)
	}
	if raw, ok := getMap(limits, "field_name_length")["limit"]; ok && !mapping.isEmpty() {
		limit := getInt(M{"v": raw}, "v", math.MaxInt32)
		if name := fieldNameOverLimit(mapping.Properties, limit); name != "" {
			return errIllegalArgument("Field name [%s] is longer than the limit of [%d] characters", name, limit)
		}
	}
	return nil
}

// deepObjectPath returns an object field whose depth (dots + 2) exceeds
// the limit, as MapperService.checkDepthLimit.
func deepObjectPath(fields map[string]*Field, limit int) string {
	var found string
	var walk func(map[string]*Field, string)
	walk = func(fields map[string]*Field, prefix string) {
		for _, name := range sortedFieldNames(fields) {
			field := fields[name]
			if found != "" {
				return
			}
			path := prefix + name
			if field.Type == TypeObject || field.Type == TypeNested {
				if strings.Count(path, ".")+2 > limit {
					found = path
					return
				}
				walk(field.Properties, path+".")
			}
		}
	}
	walk(fields, "")
	return found
}

// utf16Length is String.length().
func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

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

func errObjectConcrete(full, name string) *Error {
	return errMapperParsing("object mapping for [%s] tried to parse field [%s] as object, but found a concrete value", full, name)
}

// buildNested indexes the objects of a nested field as documents of their
// own, numbered in index order below the current level.
func (b *docBuilder) buildNested(full, key string, f *Field, val any, dynamic string) error {
	root := b.rootBuilder()
	limit := b.settingsLimits().nested
	if b.nestedCount == nil {
		b.nestedCount = map[string]int{}
	}
	var objects []any
	switch t := val.(type) {
	case M:
		objects = []any{t}
	case []any:
		objects = flattenValues(t)
	default:
		return errObjectConcrete(full, key)
	}
	for _, e := range objects {
		m, ok := e.(M)
		if !ok {
			return errObjectConcrete(full, "null")
		}
		root.nestedTotal++
		if root.nestedTotal > limit {
			return errMapperParsing("The number of nested documents has exceeded the allowed limit of [%d]. This limit can be set by changing the [index.mapping.nested_objects.limit] index level setting.", limit)
		}
		off := b.nestedCount[full]
		b.nestedCount[full]++
		cb := &docBuilder{ix: b.ix, src: b.src, id: nestedID(b.id, full, off), level: full, exists: map[string]bool{}, infer: b.infer, root: root, body: b.body, parent: b}
		cb.doc = document.NewDocument(cb.id)
		if err := cb.walkObject(full+".", m, f.Properties, dynamic, f.Dynamic, nil); err != nil {
			return err
		}
		if err := cb.finish(); err != nil {
			return err
		}
		cb.doc.AddField(document.NewTextFieldCustom(fieldNestedPath, nil, []byte(full), index.IndexField, b.ix.keywordAnalyzer()))
		cb.doc.AddField(document.NewCompositeFieldWithIndexingOptions("_all", true, nil, []string{"_exists_"}, index.IndexField|index.IncludeTermVectors))
		root.children = append(root.children, cb.doc)
		root.pending = append(root.pending, cb.pending...)
		if err := b.includeNested(full, f, m, dynamic); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) keywordAnalyzer() analysis.Analyzer {
	a, _ := ix.analysis.analyzerNamed("keyword")
	return a
}

// validateFieldName applies DocumentParser's checks of field names.
func validateFieldName(key string) *Error {
	if key == "" {
		return errFailedToParse(errIllegalArgument("field name cannot be an empty string"))
	}
	if !strings.Contains(key, ".") {
		return nil
	}
	parts := splitJavaPath(key)
	if len(parts) == 0 {
		return errFailedToParse(errIllegalArgument("field name cannot contain only the character [.]"))
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			if p != "" {
				return errFailedToParse(errIllegalArgument("object field cannot contain only whitespace: ['%s']", key))
			}
			return errFailedToParse(errIllegalArgument("object field starting or ending with a [.] makes object resolution ambiguous: [%s]", key))
		}
	}
	return nil
}

func errStrictDynamicWithin(key, prefix string) *Error {
	parent := strings.TrimSuffix(prefix, ".")
	if parent == "" {
		parent = "_doc"
	}
	return &Error{Status: 400, Type: "strict_dynamic_mapping_exception", Reason: "mapping set to strict, dynamic introduction of [" + key + "] within [" + parent + "] is not allowed"}
}

func containsObject(v any) bool {
	switch t := v.(type) {
	case M:
		return true
	case []any:
		for _, e := range t {
			if containsObject(e) {
				return true
			}
		}
	}
	return false
}

// walkObject indexes the fields of an object. dynamic is the effective
// dynamic setting, explicitDynamic the object's own one.
func (b *docBuilder) walkObject(prefix string, obj M, fields map[string]*Field, dynamic, explicitDynamic string, arrayPos []uint64) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	depthLimit := b.settingsLimits().depth
	for _, key := range keys {
		if err := validateFieldName(key); err != nil {
			return err
		}
		val := obj[key]
		full := prefix + key
		if containsObject(val) && strings.Count(full, ".")+2 > depthLimit {
			return errFailedToParse(&Error{Type: "parse_exception", Reason: fmt.Sprintf("The depth of the field has exceeded the allowed limit of [%d]. This limit can be set by changing the [index.mapping.depth.limit] index level setting.", depthLimit)})
		}
		f, ok := fields[key]
		if !ok && b.shadow {
			// a shadow builder sees the fields this document introduced
			if f = b.rootBuilder().pendingField(full); f == nil {
				continue
			}
			ok = true
		}
		if !ok && val == nil {
			if explicitDynamic == "strict" || explicitDynamic == "strict_allow_templates" {
				return errStrictDynamicMode(key, prefix, explicitDynamic)
			}
			continue
		}
		if !ok {
			switch dynamic {
			case "false":
				continue
			case "strict":
				return errStrictDynamicWithin(key, prefix)
			}
			if !b.infer {
				continue
			}
			var err error
			if dynamic == "strict_allow_templates" || dynamic == "false_allow_templates" {
				// only the fields matching a dynamic template are introduced
				var matched bool
				if f, matched, err = b.ix.Mapping.templateField(full, val); err != nil {
					return err
				}
				if !matched {
					if dynamic == "strict_allow_templates" {
						return errStrictDynamicMode(key, prefix, dynamic)
					}
					continue
				}
			} else if f, err = b.ix.Mapping.inferTreeForPath(full, val); err != nil {
				if c, isConflict := err.(*dynamicTypeConflict); isConflict {
					return b.conflictError(c)
				}
				return err
			}
			if f == nil {
				continue
			}
			b.pending = append(b.pending, pendingField{target: fields, name: key, path: full, field: f})
		}
		if val == nil && f.NullValue == nil && f.Type != TypeConstantKeyword && f.Type != TypeRankFeatures && f.Type != TypeJoin {
			continue
		}
		if err := b.walkField(full, key, f, val, dynamic, arrayPos); err != nil {
			return err
		}
	}
	return nil
}

// acceptsObjects reports the field types whose values are JSON objects.
func acceptsObjects(f *Field) bool {
	switch f.Type {
	case TypeGeoPoint, TypeGeoShape, TypeXYPoint, TypeXYShape, TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange,
		TypeDateRange, TypeIPRange, TypeFlatObject, TypeCompletion, TypeJoin, TypeRankFeatures, TypePercolator, TypeKNNVector:
		return true
	}
	return false
}

func (b *docBuilder) walkField(full, key string, f *Field, val any, dynamic string, arrayPos []uint64) error {
	switch f.Type {
	case TypeObject, TypeNested:
		if !f.Enabled {
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
			if b.shadow {
				// the fields of nested objects included in their parent
				// belong to the enclosing document as well
				if !getBool(f.Extra, "include_in_parent", false) {
					return nil
				}
				for _, m := range nestedObjects(val) {
					if err := b.walkObject(full+".", m, f.Properties, d, f.Dynamic, arrayPos); err != nil {
						return err
					}
				}
				return nil
			}
			// nested objects live in documents of their own; the parent
			// keeps no trace of them, as on OpenSearch
			return b.buildNested(full, key, f, val, d)
		}
		switch t := val.(type) {
		case M:
			return b.walkObject(full+".", t, f.Properties, d, f.Dynamic, arrayPos)
		case []any:
			for i, e := range t {
				if e == nil {
					continue
				}
				m, ok := e.(M)
				if !ok {
					return errObjectConcrete(full, "null")
				}
				if err := b.walkObject(full+".", m, f.Properties, d, f.Dynamic, append(append([]uint64(nil), arrayPos...), uint64(i))); err != nil {
					return err
				}
			}
			return nil
		default:
			return errObjectConcrete(full, key)
		}
	case TypeAlias:
		return errFailedToParse(errIllegalArgument("Cannot write to a field alias [%s].", full))
	}
	if _, isObj := val.(M); isObj && !acceptsObjects(f) {
		parent := strings.TrimSuffix(strings.TrimSuffix(full, key), ".")
		if dotted := rawDottedKey(b.src.Raw, parent, key); dotted != "" {
			name := dotted
			if parent != "" {
				name = parent + "." + dotted
			}
			return errMapperParsing("Could not dynamically add mapping for field [%s]. Existing mapping for [%s] must be of type object but found [%s].", name, full, f.Type)
		}
	}
	counters := b.rootBuilder().occ
	if b.shadow {
		counters = b.occ
	}
	vals := leafValues(f, val)
	for i, v := range vals {
		occ := counters[full]
		counters[full]++
		if v == nil && f.Type != TypeRankFeatures && f.Type != TypeJoin {
			if f.Type == TypeConstantKeyword {
				return b.valueError(full, f, nil, "null", errIllegalArgument("constant keyword field [%s] must have a value", full))
			}
			if f.NullValue == nil {
				continue
			}
			v = f.NullValue
		}
		pos := arrayPos
		if len(vals) > 1 || len(arrayPos) > 0 {
			pos = append(append([]uint64(nil), arrayPos...), uint64(i))
		}
		indexed, err := b.addLeaf(full, full, f, v, pos, occ)
		if err != nil {
			return err
		}
		if indexed {
			b.markExists(full)
		}
		for _, sn := range sortedFieldNames(f.Fields) {
			subIndexed, err := b.addLeaf(full+"."+sn, full, f.Fields[sn], v, pos, occ)
			if err != nil {
				return err
			}
			if subIndexed {
				b.exists[full+"."+sn] = true
			}
		}
		if b.shadow {
			continue
		}
		for _, target := range f.CopyTo {
			// the copy goes to the document of the target's nested level
			tb := b.copyToBuilder(target)
			if tb.copyTo == nil {
				tb.copyTo = map[string][]any{}
			}
			tb.copyTo[target] = append(tb.copyTo[target], v)
		}
	}
	return nil
}

// leafValues lists the values of a leaf field, nulls included. geo_point
// arrays ([lon, lat]) and vectors are kept as one value.
func leafValues(f *Field, v any) []any {
	if f != nil {
		switch f.Type {
		case TypeGeoPoint, TypeXYPoint:
			if arr, ok := v.([]any); ok {
				if len(arr) > 0 && isNumberValue(arr[0]) {
					return []any{v}
				}
				var out []any
				for _, e := range arr {
					out = append(out, leafValues(f, e)...)
				}
				return out
			}
			return []any{v}
		case TypeKNNVector, TypeCompletion:
			return []any{v}
		}
	}
	return flattenKeepNull(v)
}

func isNumberValue(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return true
	}
	return false
}

func flattenKeepNull(v any) []any {
	if t, ok := v.([]any); ok {
		var out []any
		for _, e := range t {
			out = append(out, flattenKeepNull(e)...)
		}
		return out
	}
	return []any{v}
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

// valueError is the mapper_parsing_exception of a value a field could not
// parse.
func (b *docBuilder) valueError(name string, f *Field, v any, preview string, cause *Error) *Error {
	switch f.Type {
	case TypeGeoPoint, TypeXYPoint:
		return &Error{Status: 400, Type: "mapper_parsing_exception", Reason: fmt.Sprintf("failed to parse field [%s] of type [%s]", name, f.Type), Cause: cause}
	}
	if preview == "" {
		preview = javaValueString(v)
	}
	return &Error{Status: 400, Type: "mapper_parsing_exception",
		Reason: fmt.Sprintf("failed to parse field [%s] of type [%s] in document with id '%s'. Preview of field's value: '%s'", name, f.Type, b.src.ID, preview), Cause: cause}
}

// withLocation adds the Jackson location to an input coercion failure.
func (b *docBuilder) withLocation(e *Error, rawPath string, occ int, closing bool) *Error {
	off := rawTokenEnd(b.locationSource(), rawPath, occ, closing)
	if off < 0 && closing {
		off = rawTokenEnd(b.locationSource(), rawPath, 0, closing)
	}
	if off < 0 {
		off = 0
	}
	reason := e.Reason + jacksonLocation(off)
	return &Error{Type: e.Type, Reason: reason, Cause: &Error{Type: e.Type, Reason: reason}}
}

// leafText is XContentParser.text() of a value: objects have no text.
func (b *docBuilder) leafText(v any, rawPath string, occ int) (string, *Error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case M:
		raw := b.locationSource()
		line, col := 1, 0
		if off := rawTokenEnd(raw, rawPath, occ, false); off > 0 {
			// the location of the START_OBJECT token, which ends at off
			line, col = jacksonLineCol(raw, off-1)
		}
		return "", &Error{Type: "illegal_state_exception", Reason: fmt.Sprintf("Can't get text on a START_OBJECT at %d:%d", line, col)}
	}
	return fmt.Sprint(v), nil
}

// ignoreMalformed reports whether malformed values of a field are ignored
// (the field parameter or index.mapping.ignore_malformed).
func (ix *Index) ignoreMalformed(f *Field) bool {
	switch f.Type {
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat, TypeScaledFloat, TypeUnsignedLong,
		TypeDate, TypeDateNanos, TypeIP, TypeGeoPoint,
		TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange, TypeDateRange, TypeIPRange:
	default:
		return false
	}
	if raw, ok := f.Extra["ignore_malformed"]; ok {
		return getBool(M{"v": raw}, "v", false)
	}
	return getBool(getMap(getMap(ix.Settings, "index"), "mapping"), "ignore_malformed", false)
}

var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// addLeaf parses and indexes one value of a leaf field. It reports whether
// the document now has a value for the field (index or doc values).
func (b *docBuilder) addLeaf(name, rawPath string, f *Field, v any, arrayPos []uint64, occ int) (bool, error) {
	ix := b.ix
	opts := index.IndexField | index.IncludeTermVectors
	indexed := fieldIndexed(f)
	fail := func(cause *Error, preview string) (bool, error) {
		if ix.ignoreMalformed(f) {
			b.rootBuilder().ignored[name] = true
			return false, nil
		}
		return false, b.valueError(name, f, v, preview, cause)
	}
	switch f.Type {
	case TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange, TypeDateRange, TypeIPRange:
		return b.addRange(name, rawPath, f, v, occ)
	case TypeKNNVector:
		return b.addKNNVector(name, rawPath, f, v, occ)
	case TypeCompletion:
		return b.addCompletion(name, rawPath, f, v, occ)
	case TypeFlatObject:
		return b.addFlatObject(name, rawPath, f, v, arrayPos, occ)
	case TypeRankFeature:
		return b.addRankFeature(name, rawPath, f, v, occ)
	case TypeRankFeatures:
		return b.addRankFeatures(name, rawPath, f, v, occ)
	case TypeJoin:
		return b.addJoin(name, rawPath, f, v, occ)
	case TypeText, TypeSearchAsYouType, TypeMatchOnlyText:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			return b.leafFailure(name, f, v, "", cerr)
		}
		if !f.Index {
			return false, nil
		}
		an, err := ix.analysis.analyzerNamed(f.Analyzer)
		if err != nil {
			return false, err
		}
		b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(s), opts, an))
		if f.Type == TypeSearchAsYouType {
			b.addSaytSubfields(name, arrayPos, s, opts, an, f)
		}
		return true, nil
	case TypeKeyword, TypeWildcard, TypeVersion:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			return b.leafFailure(name, f, v, "", cerr)
		}
		if f.Type == TypeVersion && !semverRe.MatchString(s) {
			return b.leafFailure(name, f, v, "", errIllegalArgument("Invalid semantic version format: [%s]", s))
		}
		if (f.IgnoreAbove > 0 || f.ignoreAboveSet && f.IgnoreAbove == 0) && utf8.RuneCountInString(s) > f.IgnoreAbove {
			return false, nil
		}
		if !indexed {
			return false, nil
		}
		an, err := ix.analysis.normalizerNamed(f.Normalizer)
		if err != nil {
			return false, err
		}
		b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(s), opts, an))
		return true, nil
	case TypeConstantKeyword:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			return b.leafFailure(name, f, v, "", cerr)
		}
		if want := getString(f.Extra, "value"); s != want {
			return b.leafFailure(name, f, v, "", errIllegalArgument("constant keyword field [%s] must have a value of [%s]", name, want))
		}
		return false, nil
	case TypeIP:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			if ix.ignoreMalformed(f) {
				// ignore_malformed skips objects without recording them
				return false, nil
			}
			return false, b.valueError(name, f, v, "", cerr)
		}
		ip, ok := parseIPString(s)
		if !ok {
			return fail(errNotIP(s), "")
		}
		if !indexed {
			return false, nil
		}
		b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(ipTerm(ip)), opts, ix.keywordAnalyzer()))
		return true, nil
	case TypeTokenCount:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			return b.leafFailure(name, f, v, "", cerr)
		}
		n, err := ix.tokenCount(f, s)
		if err != nil {
			return false, err
		}
		if !indexed {
			return false, nil
		}
		b.doc.AddField(document.NewNumericFieldWithIndexingOptions(name, arrayPos, float64(n), index.IndexField))
		b.doc.AddField(document.NewTextFieldCustom(exactNumericField(name), arrayPos, []byte(strconv.Itoa(n)), index.IndexField, ix.keywordAnalyzer()))
		return true, nil
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeDouble, TypeFloat, TypeHalfFloat, TypeScaledFloat, TypeUnsignedLong:
		nv, preview, cerr := parseNumericField(f, v, b.coerceEnabled(f))
		if cerr != nil && ix.ignoreMalformed(f) {
			if _, isObj := v.(M); isObj {
				// numbers skip objects without recording them (scaled_float
				// leaves the parser inside the object)
				if f.Type == TypeScaledFloat {
					node, _ := b.rawLeaf(rawPath, occ, false)
					return false, b.errLeftoverContent(node)
				}
				return false, nil
			}
			if f.Type == TypeUnsignedLong && strings.Contains(cerr.Reason, "is out of range for an unsigned long") {
				return false, nil
			}
		}
		if cerr != nil {
			if cerr.Type == "input_coercion_exception" {
				cerr = b.withLocation(cerr, rawPath, occ, false)
			}
			return fail(cerr, preview)
		}
		if nv.null {
			if f.NullValue == nil {
				return false, nil
			}
			if nv, _, cerr = parseNumericField(f, f.NullValue, true); cerr != nil || nv.null {
				return false, nil
			}
		}
		if !indexed {
			return false, nil
		}
		b.doc.AddField(document.NewNumericFieldWithIndexingOptions(name, arrayPos, nv.value, index.IndexField))
		if nv.exact != "" {
			b.doc.AddField(document.NewTextFieldCustom(exactNumericField(name), arrayPos, []byte(nv.exact), index.IndexField, ix.keywordAnalyzer()))
		}
		return true, nil
	case TypeBoolean:
		bv, cerr := parseBooleanField(v)
		if cerr != nil {
			if cerr.Type == "input_coercion_exception" {
				cerr = b.withLocation(cerr, rawPath, occ, false)
			}
			return b.leafFailure(name, f, v, "", cerr)
		}
		if !indexed {
			return false, nil
		}
		b.doc.AddField(document.NewBooleanFieldWithIndexingOptions(name, arrayPos, bv, index.IndexField))
		return true, nil
	case TypeDate, TypeDateNanos:
		s, cerr := b.leafText(v, rawPath, occ)
		if cerr != nil {
			if ix.ignoreMalformed(f) {
				// ignore_malformed skips objects without recording them
				return false, nil
			}
			return false, b.valueError(name, f, v, "", cerr)
		}
		df := f.Format
		if df == nil {
			df = defaultDateFormat
		}
		res, de := df.parseDate(s, false, time.UTC)
		if de != nil {
			return fail(de.cause(), "")
		}
		if f.Type == TypeDateNanos {
			if msg := nanosRangeError(res.t); msg != "" {
				return fail(errIllegalArgument("%s", msg), "")
			}
		}
		if b.level == "" {
			// a nested field's values would mix across nested objects here,
			// so only the root document's own fields are staged; committed to
			// src.dateCache only once the whole document has built
			// successfully (see buildDocumentFrom), so a rebuild (segments.go,
			// Index.rebuild) that re-walks an already-stored document
			// replaces its cache instead of doubling up on it
			root := b.rootBuilder()
			if root.dateCache == nil {
				root.dateCache = map[string][]time.Time{}
			}
			root.dateCache[name] = append(root.dateCache[name], res.t)
		}
		if !indexed {
			return false, nil
		}
		if f.Type == TypeDateNanos {
			fld, err := document.NewDateTimeFieldWithIndexingOptions(name, arrayPos, res.t, "", index.IndexField)
			if err != nil {
				return fail(errIllegalArgument("%s", err.Error()), "")
			}
			b.doc.AddField(fld)
		} else {
			b.doc.AddField(document.NewNumericFieldWithIndexingOptions(name, arrayPos, float64(epochMillis(res.t)), index.IndexField))
		}
		return true, nil
	case TypeGeoPoint:
		lat, lon, gerr := parseGeoPointValue(v, getBool(f.Extra, "ignore_z_value", true))
		if gerr != nil {
			if gerr.Type == "input_coercion_exception" {
				gerr = b.withLocation(gerr, rawPath, occ, true)
			}
			return fail(gerr, "")
		}
		if rerr := checkGeoRange(lat, lon, name); rerr != nil {
			return fail(rerr, "")
		}
		if !indexed {
			return false, nil
		}
		b.doc.AddField(document.NewGeoPointFieldWithIndexingOptions(name, arrayPos, lon, lat, index.IndexField))
		return true, nil
	case TypeBinary:
		stored := getBool(f.Extra, "doc_values", false) || getBool(f.Extra, "store", false)
		if _, isObj := v.(M); isObj && !stored {
			// the value is never read: the parser stays inside the object
			node, _ := b.rawLeaf(rawPath, occ, false)
			return false, b.errLeftoverContent(node)
		}
		return stored, nil
	}
	// other types (ranges, vectors, completion, ...) are stored
	return true, nil
}

// tokenCount is TokenCountFieldMapper.countPositions.
func (ix *Index) tokenCount(f *Field, s string) (int, error) {
	an, err := ix.analysis.analyzerNamed(f.Analyzer)
	if err != nil {
		return 0, err
	}
	ts := an.Analyze([]byte(s))
	if !getBool(f.Extra, "enable_position_increments", true) {
		return len(ts), nil
	}
	max := 0
	for _, t := range ts {
		if t.Position > max {
			max = t.Position
		}
	}
	return max, nil
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
	b, err := parseBooleanField(v)
	return b, err == nil
}

// geoPointValue parses the geo_point representations: {"lat":..,"lon":..},
// "lat,lon", [lon, lat], "POINT (lon lat)", geohashes and GeoJSON points.
func geoPointValue(v any) (lat, lon float64, ok bool) {
	lat, lon, err := parseGeoPointValue(v, true)
	return lat, lon, err == nil
}

// value extraction for sorting and aggregations -----------------------

// fieldValues returns the values of a field in a document, converted to the
// mapped type: string for keyword/text, float64 for numbers, bool, and
// time.Time for dates. path may address a multi-field (title.keyword).
func (ix *Index) fieldValues(d *Doc, path string) []any {
	return ix.fieldValuesAt(d, path, false)
}

// fieldValuesResolved is fieldValues with the mapping already resolved (f
// and base from Mapping.resolve of path; f nil when the path is unmapped),
// for callers that read the same field of many documents, such as
// aggregations. It mirrors fieldValuesWith for the doc values case.
func (ix *Index) fieldValuesResolved(d *Doc, path string, f *Field, base string) []any {
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
	if f == nil || !ix.Mapping.visibleAt(base, d.level()) || isRangeType(f.Type) {
		return nil
	}
	if f.Type == TypeConstantKeyword {
		return []any{ix.Name}
	}
	raw, found := lookupPathFound(d.Src, base)
	var vals []any
	if found {
		vals = leafValues(f, raw)
	}
	vals = append(vals, ix.copiedValues(d, path)...)
	if len(vals) == 0 {
		return nil
	}
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		if s, isString := v.(string); isString && s == "" && f.isNumeric() {
			v = nil
		}
		if v == nil {
			if f.NullValue == nil {
				continue
			}
			v = f.NullValue
		}
		if f.Type == TypeTokenCount {
			if s, err := stringValue("", f, v); err == nil {
				if n, err := ix.tokenCount(f, s); err == nil {
					out = append(out, float64(n))
				}
			}
			continue
		}
		if cv, ok := convertValue(f, v); ok {
			out = append(out, cv)
		}
	}
	return out
}

// storedFieldValues returns the stored fields of a get. Names are exact
// field names (a get does not expand wildcards); values use the stored
// field formats (see storedFieldOutput).
func (ix *Index) storedFieldValues(d *Doc, fields []string) M {
	out := M{}
	for _, name := range fields {
		if name == "" || strings.HasPrefix(name, "_") {
			continue
		}
		f, _, ok := ix.Mapping.resolve(name)
		if !ok || f.Type == TypeObject || f.Type == TypeNested || !getBool(f.Extra, "store", false) {
			continue
		}
		if vals := ix.fieldValues(d, name); len(vals) > 0 {
			out[name] = storedFieldOutput(f, vals)
		}
	}
	return out
}

// fieldValuesAt is fieldValues with anyLevel reading the values of nested
// fields straight from the source whatever the document's level (the
// fields option of a search does that on OpenSearch; doc values, sorts and
// aggregations do not).
func (ix *Index) fieldValuesAt(d *Doc, path string, anyLevel bool) []any {
	return ix.fieldValuesWith(d, path, anyLevel, convertValue)
}

// fieldValuesWith is fieldValuesAt with the conversion of the values.
func (ix *Index) fieldValuesWith(d *Doc, path string, anyLevel bool, conv func(*Field, any) (any, bool)) []any {
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
	// root (and root fields are not visible from a nested object), unless
	// the nested objects are included in their parents
	if !anyLevel && !ix.Mapping.visibleAt(base, d.level()) {
		return nil
	}
	if !anyLevel && isRangeType(f.Type) {
		return nil
	}
	if !anyLevel && f.Type == TypeConstantKeyword {
		// the field data of constant_keyword fields holds the index name
		return []any{ix.Name}
	}
	raw, found := lookupPathFound(d.Src, base)
	var vals []any
	if found {
		vals = leafValues(f, raw)
	}
	if !anyLevel {
		// doc values hold the values copied into the field
		vals = append(vals, ix.copiedValues(d, path)...)
	}
	if len(vals) == 0 {
		return nil
	}
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		if s, isString := v.(string); isString && s == "" && f.isNumeric() && !anyLevel {
			// an empty string is a null number
			v = nil
		}
		if v == nil {
			if f.NullValue == nil {
				continue
			}
			v = f.NullValue
		}
		if f.Type == TypeTokenCount {
			if s, err := stringValue("", f, v); err == nil {
				if n, err := ix.tokenCount(f, s); err == nil {
					out = append(out, float64(n))
				}
			}
			continue
		}
		if cv, ok := conv(f, v); ok {
			out = append(out, cv)
		}
	}
	return out
}

func convertValue(f *Field, v any) (any, bool) {
	switch {
	case f.isNumeric():
		if _, isBool := v.(bool); isBool {
			return nil, false
		}
		n, ok := toFloat(v)
		return n, ok
	case f.isDate():
		df := f.Format
		if df == nil {
			df = defaultDateFormat
		}
		t, err := df.Parse(v)
		return t, err == nil
	case f.Type == TypeBoolean:
		b, err := parseBooleanField(v)
		return b, err == nil
	case f.Type == TypeGeoPoint:
		lat, lon, err := parseGeoPointValue(v, true)
		return [2]float64{lat, lon}, err == nil
	case f.Type == TypeIP:
		s, err := stringValue("", f, v)
		if err != nil {
			return nil, false
		}
		return normalizeIPValue(s)
	case f.Type == TypeObject, f.Type == TypeNested, isRangeType(f.Type):
		return nil, false
	case f.Type == TypeJoin:
		if m, isObj := v.(M); isObj {
			switch name := m["name"].(type) {
			case string:
				return name, true
			case []any:
				// the last name wins, as the join mapper reads the array
				for i := len(name) - 1; i >= 0; i-- {
					if s, ok := name[i].(string); ok {
						return s, true
					}
				}
			}
			return nil, false
		}
		s, isString := v.(string)
		return s, isString
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

// lookupPathFound extracts a dotted path like lookupPath and reports
// whether the path is present (an explicit null counts).
func lookupPathFound(v any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	cur, ok := v.(M)
	if !ok {
		r := lookupPath(v, path)
		return r, r != nil
	}
	for i, p := range parts {
		next, exists := cur[p]
		if !exists {
			return nil, false
		}
		if i == len(parts)-1 {
			return next, true
		}
		if m, isObj := next.(M); isObj {
			cur = m
			continue
		}
		r := lookupParts(next, parts[i+1:])
		return r, r != nil
	}
	return nil, false
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
