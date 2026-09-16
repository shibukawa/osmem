package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version reported by the root endpoint.
const Version = "3.8.0"

// Cluster is an in-memory OpenSearch-compatible cluster state.
type Cluster struct {
	mu              sync.RWMutex
	scrollMu        sync.Mutex // guards scrolls and pits
	indices         map[string]*Index
	templates       map[string]*Template // composable index templates
	legacyTemplates map[string]*Template
	scrolls         map[string]*scrollState
	pits            map[string]*pitState
	clusterSettings M
	// componentTemplates holds the component templates (template.go)
	componentTemplates map[string]*ComponentTemplate
	closed             bool

	// Now returns the current time (used for date math and creation dates).
	Now func() time.Time
	// Warn receives messages about unsupported features that were ignored.
	Warn func(string)
	// Name is the cluster name.
	Name string
	// HTTPAddress is the address reported by _nodes (set by the HTTP server).
	HTTPAddress string
}

// New creates an empty cluster.
func New() *Cluster {
	return &Cluster{
		indices:            map[string]*Index{},
		templates:          map[string]*Template{},
		legacyTemplates:    map[string]*Template{},
		scrolls:            map[string]*scrollState{},
		pits:               map[string]*pitState{},
		clusterSettings:    M{"persistent": M{}, "transient": M{}},
		componentTemplates: map[string]*ComponentTemplate{},
		Now:                time.Now,
		Name:               "osmem",
	}
}

// Clone returns an independent copy of the cluster. Indices are shared until
// either side writes to them.
func (c *Cluster) Clone() *Cluster {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := New()
	n.Now = c.Now
	n.Warn = c.Warn
	n.Name = c.Name
	n.clusterSettings = cloneDeep(c.clusterSettings).(M)
	for k, ix := range c.indices {
		ix.refs.Add(1)
		n.indices[k] = ix
	}
	for k, t := range c.templates {
		n.templates[k] = t
	}
	for k, t := range c.legacyTemplates {
		n.legacyTemplates[k] = t
	}
	for k, t := range c.componentTemplates {
		n.componentTemplates[k] = t
	}
	return n
}

// Close releases the cluster's indices.
func (c *Cluster) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for k, ix := range c.indices {
		ix.release()
		delete(c.indices, k)
	}
	c.scrollMu.Lock()
	for _, pit := range c.pits {
		releasePIT(pit)
	}
	c.pits = map[string]*pitState{}
	c.scrollMu.Unlock()
}

func (c *Cluster) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cluster) warn(format string, args ...any) {
	if c.Warn != nil {
		c.Warn(fmt.Sprintf(format, args...))
	}
}

func (c *Cluster) warnFunc() func(string) {
	return func(s string) { c.warn("%s", s) }
}

// writable returns the index for writing, copying it first if it is shared
// with another cluster.
func (c *Cluster) writable(name string) (*Index, error) {
	ix, ok := c.indices[name]
	if !ok {
		return nil, errIndexNotFound(name)
	}
	if ix.refs.Load() > 1 {
		n, err := ix.copyIndex()
		if err != nil {
			return nil, err
		}
		c.indices[name] = n
		ix.release()
		return n, nil
	}
	return ix, nil
}

// index name resolution ------------------------------------------------

type target struct {
	ix     *Index
	filter M // alias filter, if any
}

// resolveOptions selects how resolve expands an expression: params are the
// request parameters applied over base (the search options when nil).
type resolveOptions struct {
	ignoreUnavailable bool
	allowNoIndices    bool
	allowAliases      bool
	params            Params
	base              *indicesOptions
}

// resolveOpts is the resolution of search-like requests: the parameters
// expand_wildcards, ignore_unavailable and allow_no_indices apply over the
// search defaults and alias filters are kept.
func resolveOpts(p Params) resolveOptions {
	return resolveOptions{allowAliases: true, allowNoIndices: true, params: p}
}

func (c *Cluster) sortedIndexNames() []string {
	names := make([]string, 0, len(c.indices))
	for n := range c.indices {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// aliasTargets returns the indices that have an alias.
func (c *Cluster) aliasTargets(alias string) []target {
	var out []target
	for _, name := range c.sortedIndexNames() {
		ix := c.indices[name]
		if a, ok := ix.Aliases[alias]; ok {
			out = append(out, target{ix: ix, filter: a.Filter})
		}
	}
	return out
}

// resolve expands an index expression into concrete targets (see
// resolve.go). Search-like requests (allowAliases without base options)
// carry the alias filters of each index.
func (c *Cluster) resolve(expr string, opts resolveOptions) ([]target, error) {
	o := searchIndicesOptions
	switch {
	case opts.base != nil:
		o = *opts.base
	case !opts.allowAliases:
		o = aliasActionOptions
	}
	if opts.params == nil {
		o.ignoreUnavailable = o.ignoreUnavailable || opts.ignoreUnavailable
		o.allowNoIndices = opts.allowNoIndices
	} else {
		var err error
		if o, err = o.withParams(opts.params); err != nil {
			return nil, err
		}
	}
	return c.newExprResolver().targets(expr, o, opts.allowAliases && opts.base == nil)
}

// resolveWriteIndex resolves an expression that must name exactly one index
// (or an alias pointing at one index / with a write index) for a document
// request. A closed index rejects document requests.
func (c *Cluster) resolveWriteIndex(name string) (*Index, error) {
	ix, err := c.writeIndex(name)
	if err != nil {
		return nil, err
	}
	if ix.stateClosed {
		return nil, errIndexClosed(ix)
	}
	return ix, nil
}

func (c *Cluster) writeIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		return ix, nil
	}
	ts := c.aliasTargets(name)
	switch len(ts) {
	case 0:
		return nil, errIndexNotFound(name)
	case 1:
		// an alias whose only index has is_write_index=false has no write index
		if a := ts[0].ix.Aliases[name]; a.IsWriteIndex == nil || *a.IsWriteIndex {
			return ts[0].ix, nil
		}
	}
	for _, t := range ts {
		if a := t.ix.Aliases[name]; a.IsWriteIndex != nil && *a.IsWriteIndex {
			return t.ix, nil
		}
	}
	return nil, errIllegalArgument("no write index is defined for alias [%s]. The write index may be explicitly disabled using is_write_index=false or the alias points to multiple indices without one being designated as a write index", name)
}

func (c *Cluster) validateRequireAlias(name string) error {
	if len(c.aliasTargets(name)) > 0 {
		return nil
	}
	if _, exists := c.indices[name]; exists {
		return errIllegalArgument("require_alias is true but index [%s] is not an alias", name)
	}
	return errIndexNotFound(name)
}

func validateRequiredRouting(ix *Index, id string, dp DocParams) error {
	if dp.Routing == "" && getBool(getMap(ix.Mapping.Extra, "_routing"), "required", false) {
		return &Error{Status: 400, Type: "routing_missing_exception", Reason: "routing is required for [" + ix.Name + "]/[_doc]/[" + id + "]", Index: ix.Name}
	}
	return nil
}

// index lifecycle ------------------------------------------------------

var indexNameRe = regexp.MustCompile(`^[^A-Z\\/*?"<>| ,#:]+$`)

func validateIndexName(name string) error {
	if name == "" || name == "." || name == ".." {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], must not be '.' or '..'", Index: name}
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "+") {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], must not start with '_', '-', or '+'", Index: name}
	}
	if !indexNameRe.MatchString(name) {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], must be lowercase and must not contain the following characters: [ , \", *, \\, <, |, ,, >, /, ?]", Index: name}
	}
	if len(name) > 255 {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], index name is too long, (" + strconv.Itoa(len(name)) + " > 255)", Index: name}
	}
	return nil
}

// normalizeSettings converts a settings body into {"index": {...}} form.
// Scalar values are stored as strings, the way OpenSearch reports them.
func normalizeSettings(body M) M {
	idx := M{}
	for k, v := range body {
		v = stringifySettings(v)
		switch {
		case k == "index":
			if sub, ok := v.(M); ok {
				for sk, sv := range sub {
					setNested(idx, sk, sv)
				}
			}
		case strings.HasPrefix(k, "index."):
			setNested(idx, strings.TrimPrefix(k, "index."), v)
		default:
			setNested(idx, k, v)
		}
	}
	return M{"index": idx}
}

func stringifySettings(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, e := range t {
			out[k] = stringifySettings(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = stringifySettings(e)
		}
		return out
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return v
}

func setNested(m M, key string, v any) {
	parts := strings.Split(key, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			if sub, ok := v.(M); ok {
				existing, ok := cur[p].(M)
				if !ok {
					existing = M{}
					cur[p] = existing
				}
				for sk, sv := range sub {
					setNested(existing, sk, sv)
				}
				return
			}
			cur[p] = v
			return
		}
		next, ok := cur[p].(M)
		if !ok {
			next = M{}
			cur[p] = next
		}
		cur = next
	}
}

func getNested(m M, key string) (any, bool) {
	parts := strings.Split(key, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(M)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// CreateIndex implements PUT /{index}.
func (c *Cluster) CreateIndex(name string, body M) (Response, error) {
	return c.CreateIndexWithParams(name, body, Params{})
}

// CreateIndexWithParams implements PUT /{index} with its URL parameters
// (wait_for_active_shards).
func (c *Cluster) CreateIndexWithParams(name string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateIndexName(name); err != nil {
		return fail(err)
	}
	if err := prevalidateCreateIndexBody(body); err != nil {
		return fail(err)
	}
	if existing, ok := c.indices[name]; ok {
		return fail(&Error{Status: http.StatusBadRequest, Type: "resource_already_exists_exception", Reason: "index [" + name + "/" + existing.UUID + "] already exists", Index: name})
	}
	if len(c.aliasTargets(name)) > 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], already exists as alias", Index: name})
	}
	waitFor, err := activeShardCountValue(p.Get("wait_for_active_shards"))
	if err != nil {
		return fail(err)
	}
	if err := c.checkGlobalBlock(blockMetadataWrite); err != nil {
		return fail(err)
	}
	if t := c.matchingComposableTemplate(name); t != nil && t.DataStream != nil {
		return fail(errIllegalArgument("cannot create index with name [%s], because it matches with template [%s] that creates data streams only, use create data stream api instead", name, t.Name))
	}
	ix, err := c.buildIndex(name, body)
	if err != nil {
		return fail(err)
	}
	copies := indexReplicaCount(ix) + 1
	if waitFor == activeShardsDefault {
		waitFor, _ = activeShardCountValue(getString(getMap(ix.Settings, "index"), "write.wait_for_active_shards"))
		if w, ok := getNested(ix.Settings, "index.write.wait_for_active_shards"); ok {
			waitFor, _ = activeShardCountValue(settingString(w))
		}
	}
	if waitFor > copies {
		ix.release()
		return fail(errIllegalArgument("invalid wait_for_active_shards[%d]: cannot be greater than number of shard copies [%d]", waitFor, copies))
	}
	c.indices[name] = ix
	// the single node holds one active copy of each shard; osmem answers at
	// once instead of waiting for the timeout
	acknowledged := waitFor <= 1
	if waitFor == activeShardsAll {
		acknowledged = copies == 1
	}
	return ok(M{"acknowledged": true, "shards_acknowledged": acknowledged, "index": name})
}

// prevalidateCreateIndexBody runs the checks OpenSearch applies to a create
// index request before it looks at the cluster state (so they win over
// "already exists"): body keys, unknown settings, setting values and
// dependencies.
func prevalidateCreateIndexBody(body M) error {
	for _, k := range sortedMapKeys(body) {
		switch k {
		case "settings", "mappings", "aliases":
		default:
			return errParsing("unknown key [%s] for create index", k)
		}
	}
	raw, present := body["settings"]
	if !present || raw == nil {
		return nil
	}
	sm, isObject := raw.(M)
	if !isObject {
		return errParsing("key [settings] must be an object")
	}
	request := map[string]any{}
	flattenSettingsBody(request, "", sm)
	request = normalizeIndexSettingKeys(request)
	if err := validateCreateIndexSettings(request, false); err != nil {
		return err
	}
	return validateIndexSettingRelations(request, nil)
}

const (
	activeShardsDefault = -2
	activeShardsAll     = -1
)

// activeShardCountValue is ActiveShardCount.parseString.
func activeShardCountValue(s string) (int, error) {
	switch s {
	case "", "index-setting":
		return activeShardsDefault, nil
	case "all":
		return activeShardsAll, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "cannot parse ActiveShardCount[" + s + "]",
			Cause: &Error{Type: "number_format_exception", Reason: `For input string: "` + s + `"`}}
	}
	if n < 0 {
		return 0, errIllegalArgument("shard count cannot be a negative value")
	}
	return int(n), nil
}

// buildIndex creates an index from a create-index body, applying templates.
func (c *Cluster) buildIndex(name string, body M) (*Index, error) {
	for _, k := range sortedMapKeys(body) {
		switch k {
		case "settings", "mappings", "aliases":
		default:
			return nil, errParsing("unknown key [%s] for create index", k)
		}
	}
	settings := M{}
	mappings := M{}
	var templateAliases [][]*aliasDef
	// A matching composable template (composed with its component templates)
	// replaces the entire legacy template chain.
	best := c.matchingComposableTemplate(name)
	if best != nil {
		flatTemplate, composedMappings, aliasLists := c.composeTemplate(best)
		deepMerge(settings, nestSettings(flatTemplate))
		mappings = composedMappings
		templateAliases = append(templateAliases, aliasLists...)
	} else {
		var legacy []*Template
		for _, t := range c.legacyTemplates {
			if t.matches(name) {
				legacy = append(legacy, t)
			}
		}
		sort.Slice(legacy, func(i, j int) bool { return legacy[i].Priority < legacy[j].Priority })
		for _, t := range legacy {
			deepMerge(settings, t.Settings)
			deepMerge(mappings, t.Mappings)
		}
		// aliases of higher order templates win; legacy templates keep no
		// is_write_index / is_hidden flags
		for i := len(legacy) - 1; i >= 0; i-- {
			defs, err := parseAliasObjects(legacy[i].Aliases)
			if err != nil {
				return nil, err
			}
			for _, d := range defs {
				d.isWriteIndex, d.isHidden = nil, nil
			}
			templateAliases = append(templateAliases, defs)
		}
	}
	flat := flatIndexSettings(normalizeSettings(settings))
	if raw, present := body["settings"]; present && raw != nil {
		sm, isObject := raw.(M)
		if !isObject {
			return nil, errParsing("key [settings] must be an object")
		}
		request := map[string]any{}
		flattenSettingsBody(request, "", sm)
		request = normalizeIndexSettingKeys(request)
		if err := validateCreateIndexSettings(request, true); err != nil {
			return nil, err
		}
		for k, v := range request {
			if v == nil {
				delete(flat, k)
			} else {
				flat[k] = v
			}
		}
	}
	if raw, present := body["mappings"]; present && raw != nil {
		m, isObject := raw.(M)
		if !isObject {
			return nil, errParsing("key [mappings] must be an object")
		}
		if _, typed := m["_doc"].(M); typed && len(m) == 1 {
			return nil, errIllegalArgument("The mapping definition cannot be nested under a type")
		}
		if best != nil {
			mergeTemplateMappings(mappings, m)
		} else {
			deepMerge(mappings, m)
		}
	}
	requestAliases, err := parseAliasObjects(body["aliases"])
	if err != nil {
		return nil, err
	}
	aliases, err := c.resolveIndexAliases(name, requestAliases, templateAliases)
	if err != nil {
		return nil, err
	}
	for key, def := range map[string]string{"index.number_of_shards": "1", "index.number_of_replicas": "1", "index.replication.type": "DOCUMENT"} {
		if _, ok := flat[key]; !ok {
			flat[key] = def
		}
	}
	mp, err := parseMapping(mappings)
	if err != nil {
		return nil, err
	}
	if err := validateIndexSettingRelations(flat, mp); err != nil {
		return nil, err
	}
	delete(flat, "index.uuid")
	delete(flat, "index.provided_name")
	delete(flat, "index.version.created")
	settings = nestSettings(flat)
	if err := validateMappingLimits(mp, settings); err != nil {
		return nil, err
	}
	ix, err := newIndex(name, settings, mp, c.now(), c.warnFunc())
	if err != nil {
		return nil, err
	}
	idx := settings["index"].(M)
	idx["uuid"] = ix.UUID
	if _, set := idx["creation_date"]; !set {
		idx["creation_date"] = strconv.FormatInt(ix.Created.UnixMilli(), 10)
	}
	idx["provided_name"] = name
	idx["version"] = M{"created": indexVersionCreated}
	if err := c.applyIndexAliases(ix, aliases); err != nil {
		ix.release()
		return nil, err
	}
	return ix, nil
}

func deepMerge(dst, src M) {
	for k, v := range src {
		if sv, ok := v.(M); ok {
			if dv, ok := dst[k].(M); ok {
				deepMerge(dv, sv)
				continue
			}
			dst[k] = cloneDeep(sv)
			continue
		}
		dst[k] = v
	}
}

// DeleteIndex implements DELETE /{index}. Wildcards expand to open and closed
// visible indices; aliases are rejected.
func (c *Cluster) DeleteIndex(expr string, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkAckTimeouts(p); err != nil {
		return fail(err)
	}
	indices, err := c.resolveIndices(expr, p, deleteIndexOptions)
	if err != nil {
		return fail(err)
	}
	if err := c.checkDeleteBlock(indices); err != nil {
		return fail(err)
	}
	for _, ix := range indices {
		if _, ok := c.indices[ix.Name]; ok {
			ix.release()
			delete(c.indices, ix.Name)
			c.dropPITContexts(ix.Name)
		}
	}
	return ok(M{"acknowledged": true})
}

// IndexExists implements HEAD /{index}: the expression exists when it
// resolves without error, even to no index.
func (c *Cluster) IndexExists(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := c.resolveIndices(expr, p, strictExpandOptions); err != nil {
		if e, ok := err.(*Error); ok && e.Status != http.StatusNotFound {
			return Response{Status: e.Status}, nil
		}
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

// GetIndex implements GET /{index}.
func (c *Cluster) GetIndex(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandOpenOptions})
	if err != nil {
		return fail(err)
	}
	indices := uniqueTargetIndices(ts)
	if err := c.checkBlocks(indices, blockMetadataRead); err != nil {
		return fail(err)
	}
	flatOut := p.Bool("flat_settings", false)
	out := M{}
	for _, ix := range indices {
		flat := flatIndexSettings(ix.Settings)
		entry := M{
			"aliases":  c.aliasesJSON(ix),
			"mappings": ix.Mapping.toJSON(),
			"settings": renderSettings(flat, flatOut),
		}
		if p.Bool("include_defaults", false) {
			entry["defaults"] = renderSettings(indexSettingDefaults(flat, nil), flatOut)
		}
		out[ix.Name] = entry
	}
	return ok(out)
}

// indexVersionCreated is index.version.created of OpenSearch 3.8.0.
const indexVersionCreated = "137297827"

func uniqueTargetIndices(ts []target) []*Index {
	seen := map[string]bool{}
	var out []*Index
	for _, t := range ts {
		if !seen[t.ix.Name] {
			seen[t.ix.Name] = true
			out = append(out, t.ix)
		}
	}
	return out
}

func (c *Cluster) aliasesJSON(ix *Index) M {
	out := M{}
	for name, a := range ix.Aliases {
		out[name] = c.aliasJSON(ix, name, a)
	}
	return out
}

// GetMapping implements GET /{index}/_mapping.
func (c *Cluster) GetMapping(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandOpenOptions})
	if err != nil {
		return fail(err)
	}
	if err := c.checkBlocks(uniqueTargetIndices(ts), blockMetadataRead); err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		out[t.ix.Name] = M{"mappings": t.ix.Mapping.toJSON()}
	}
	return ok(out)
}

// GetFieldMapping implements GET /{index}/_mapping/field/{fields}. Closed
// indices have no shard to answer and are left out; a response without any
// index is 404.
func (c *Cluster) GetFieldMapping(expr, fields string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandOpenOptions})
	if err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		if t.ix.stateClosed {
			continue
		}
		fm := M{}
		for _, pat := range splitList(fields) {
			// metadata fields named explicitly render their non-default
			// parameters only
			switch pat {
			case "_id", "_index", "_routing", "_source", "_seq_no", "_primary_term", "_version", "_ignored", "_field_names", "_data_stream_timestamp":
				mapping := M{}
				if extra, ok := t.ix.Mapping.Extra[pat].(M); ok && len(extra) > 0 {
					mapping = M{pat: cloneDeep(extra)}
				}
				fm[pat] = M{"full_name": pat, "mapping": mapping}
				continue
			}
			for _, path := range t.ix.Mapping.leafFields(pat) {
				f, _, ok := t.ix.Mapping.resolve(path)
				if !ok {
					continue
				}
				leaf := path[strings.LastIndex(path, ".")+1:]
				if f.shingles > 0 {
					// the subfield mappers of search_as_you_type fields are named by their full path
					leaf = path
				}
				fm[path] = M{"full_name": path, "mapping": M{leaf: f.toJSON()}}
			}
		}
		out[t.ix.Name] = M{"mappings": fm}
	}
	if len(out) == 0 && len(ts) > 0 {
		return Response{Status: http.StatusNotFound, Body: M{}}, nil
	}
	return ok(out)
}

// PutMapping implements PUT /{index}/_mapping.
func (c *Cluster) PutMapping(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &updateIndicesOptions})
	if err != nil {
		return fail(err)
	}
	if err := c.checkBlocks(uniqueTargetIndices(ts), blockMetadataWrite); err != nil {
		return fail(err)
	}
	for _, t := range ts {
		ix, err := c.writable(t.ix.Name)
		if err != nil {
			return fail(err)
		}
		// validate on a clone first so a failure leaves the mapping unchanged
		trial := ix.Mapping.clone()
		if err := trial.merge(body); err != nil {
			return fail(err)
		}
		if err := validateMappingLimits(trial, ix.Settings); err != nil {
			return fail(err)
		}
		before := ix.Mapping.nestedPaths()
		ix.Mapping = trial
		// segments written so far keep the mapping they were indexed with
		ix.mappingGen++
		// an inferred object promoted to nested moves its fields into
		// documents of their own: re-index
		if after := trial.nestedPaths(); strings.Join(after, ",") != strings.Join(before, ",") {
			if err := ix.rebuild(); err != nil {
				return fail(err)
			}
		}
	}
	return ok(M{"acknowledged": true})
}

// GetSettings implements GET /{index}/_settings.
func (c *Cluster) GetSettings(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandOpenOptions})
	if err != nil {
		return fail(err)
	}
	indices := uniqueTargetIndices(ts)
	if err := c.checkBlocks(indices, blockMetadataRead); err != nil {
		return fail(err)
	}
	names := splitList(p.Get("settings_filter"))
	if len(names) == 1 && (names[0] == "_all" || names[0] == "*") {
		names = nil
	}
	includeDefaults := p.Bool("include_defaults", false)
	flatOut := p.Bool("flat_settings", false)
	out := M{}
	for _, ix := range indices {
		flat := flatIndexSettings(ix.Settings)
		selected := map[string]any{}
		for k, v := range flat {
			if settingNamesMatch(names, k) {
				selected[k] = v
			}
		}
		if !includeDefaults {
			// indices without matching settings are left out
			if len(selected) > 0 {
				out[ix.Name] = M{"settings": renderSettings(selected, flatOut)}
			}
			continue
		}
		out[ix.Name] = M{"settings": renderSettings(selected, flatOut), "defaults": renderSettings(indexSettingDefaults(flat, names), flatOut)}
	}
	return ok(out)
}

// PutSettings implements PUT /{index}/_settings (RestUpdateSettingsAction and
// MetadataUpdateSettingsService).
func (c *Cluster) PutSettings(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source := body
	if inner, wrapped := body["settings"].(M); wrapped {
		source = inner
	}
	flat := map[string]any{}
	flattenSettingsBody(flat, "", source)
	flat = normalizeIndexSettingKeys(flat)
	if len(flat) == 0 {
		return fail(errActionRequestValidation("no settings to update"))
	}
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &updateIndicesOptions})
	if err != nil {
		return fail(err)
	}
	indices := uniqueTargetIndices(ts)
	if err := c.checkGlobalBlock(blockMetadataWrite); err != nil {
		return fail(err)
	}
	// changes to the metadata / read-only blocks are allowed so that an index
	// can be unblocked
	_, metadataBlock := flat["index.blocks.metadata"]
	_, readOnly := flat["index.blocks.read_only"]
	_, readOnlyAllowDelete := flat["index.blocks.read_only_allow_delete"]
	if !((len(flat) == 1 && metadataBlock) || readOnly || readOnlyAllowDelete) {
		if err := c.checkBlocks(indices, blockMetadataWrite); err != nil {
			return fail(err)
		}
	}
	keys := sortedFlatKeys(flat)
	var static []string
	for _, key := range keys {
		value := flat[key]
		if value == nil && strings.Contains(key, "*") {
			continue
		}
		def, known := lookupIndexSetting(key)
		if !known {
			return fail(errUnknownIndexSetting(key))
		}
		if def.internal() {
			return fail(errSettings("can not update internal setting [" + key + "]; this setting is managed via a dedicated API"))
		}
		if def.private() {
			return fail(errSettings("can not update private setting [" + key + "]; this setting is managed by OpenSearch"))
		}
		if err := validateSetting(key, def, value); err != nil {
			return fail(err)
		}
		if !def.dynamic() {
			static = append(static, key)
		}
	}
	// non-dynamic settings change only on closed indices, final settings
	// never; analysis changes rebuild the analyzers when the index reopens
	var open []string
	for _, ix := range indices {
		if !ix.stateClosed {
			open = append(open, "["+ix.Name+"/"+ix.UUID+"]")
		}
	}
	rebuildAnalysis := false
	for _, key := range static {
		if def, _ := lookupIndexSetting(key); def.final() && len(open) == 0 && len(indices) > 0 {
			return fail(&Error{Status: http.StatusBadRequest, Type: "settings_exception", Reason: "final " + indices[0].Name + " setting [" + key + "], not updateable"})
		}
		if strings.HasPrefix(key, "index.analysis.") {
			rebuildAnalysis = true
		}
	}
	if len(static) > 0 && len(open) > 0 {
		return fail(errIllegalArgument("Can't update non dynamic settings [[%s]] for open indices [%s]", strings.Join(javaHashSetOrder(static), ", "), strings.Join(open, ", ")))
	}
	preserve := p.Bool("preserve_existing", false)
	for _, target := range indices {
		ix, err := c.writable(target.Name)
		if err != nil {
			return fail(err)
		}
		current := flatIndexSettings(ix.Settings)
		for _, key := range keys {
			value := flat[key]
			if value == nil {
				for k := range current {
					if simpleMatch(key, k) {
						delete(current, k)
					}
				}
				continue
			}
			if _, exists := current[key]; exists && preserve {
				continue
			}
			current[key] = value
		}
		ix.Settings = nestSettings(current)
		if rebuildAnalysis {
			ix.reopenRebuild = true
		}
	}
	return ok(M{"acknowledged": true})
}

// stats metrics of RestIndicesStatsAction and the section each one renders
var statsMetricSections = map[string]string{
	"docs": "docs", "store": "store", "indexing": "indexing", "get": "get", "search": "search", "suggest": "search",
	"merge": "merges", "refresh": "refresh", "flush": "flush", "warmer": "warmer", "query_cache": "query_cache",
	"fielddata": "fielddata", "completion": "completion", "segments": "segments", "translog": "translog",
	"request_cache": "request_cache", "recovery": "recovery",
}

// newStatsSections returns zeroed stats sections of one shard copy.
func newStatsSections() M {
	var m M
	dec := json.NewDecoder(strings.NewReader(indexStatsSections))
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

// IndexStats implements GET /{index}/_stats[/{metric}] (level, fields,
// completion_fields, fielddata_fields, include_segment_file_sizes). osmem
// tracks documents and operations; other counters are zero.
func (c *Cluster) IndexStats(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// metric and level are validated by the REST layer (prepareStats, statsPost)
	sections := map[string]bool{}
	for _, name := range splitList(p.Get("metric")) {
		if section, known := statsMetricSections[name]; known {
			sections[section] = true
		}
	}
	level := p.Get("level")
	if level != "cluster" && level != "shards" {
		level = "indices"
	}
	indices, err := c.resolveIndices(expr, p, searchIndicesOptions)
	if err != nil {
		return fail(err)
	}
	if err := c.checkBlocks(indices, blockMetadataRead); err != nil {
		return fail(err)
	}
	select_ := func(all M) M {
		out := M{}
		for k, v := range all {
			if len(sections) == 0 || sections[k] {
				out[k] = v
			}
		}
		if f, ok := out["fielddata"].(M); ok && (p.Has("fielddata_fields") || p.Has("fields")) {
			f["fields"] = M{}
		}
		if f, ok := out["completion"].(M); ok && (p.Has("completion_fields") || p.Has("fields")) {
			f["fields"] = M{}
		}
		if s, ok := out["segments"].(M); ok && p.Bool("include_segment_file_sizes", false) {
			s["file_sizes"] = M{}
		}
		return out
	}
	allPrimaries, allTotal := M{}, M{}
	indicesOut := M{}
	for _, ix := range indices {
		shardCount := indexShardCount(ix)
		stats := select_(c.indexStatsSections(ix))
		addStats(allPrimaries, stats)
		addStats(allTotal, stats)
		entry := M{"uuid": ix.UUID, "primaries": stats, "total": cloneDeep(stats)}
		if level == "shards" {
			shards := M{}
			info := catIndexInfo(ix)
			for s := 0; s < shardCount && s < len(info.Shards); s++ {
				shard := info.Shards[s]
				shardStats := select_(shardStatsSections(shard))
				var extras M
				_ = json.Unmarshal([]byte(shardStatsExtras), &extras)
				extras["routing"] = M{"state": "STARTED", "primary": true, "node": "osmem-node", "relocating_node": nil}
				if commit, ok := extras["commit"].(M); ok {
					commit["id"] = ix.UUID
					commit["num_docs"] = shard.Docs
				}
				seqNo := shard.MaxSeqNo
				extras["seq_no"] = M{"max_seq_no": seqNo, "local_checkpoint": seqNo, "global_checkpoint": seqNo}
				extras["retention_leases"] = M{"primary_term": 1, "version": 1, "leases": []any{M{"id": "peer_recovery/osmem-node", "retaining_seq_no": seqNo + 1, "timestamp": ix.Created.UnixMilli(), "source": "peer recovery"}}}
				for k, v := range extras {
					shardStats[k] = v
				}
				shards[strconv.Itoa(s)] = []any{shardStats}
			}
			entry["shards"] = shards
		}
		indicesOut[ix.Name] = entry
	}
	out := M{"_shards": broadcastShards(indices), "_all": M{"primaries": allPrimaries, "total": allTotal}}
	if level != "cluster" {
		out["indices"] = indicesOut
	}
	return ok(out)
}

// indexStatsSections fills the counters osmem knows about, using the same
// per-shard document placement and store estimate as the cat APIs (nested
// documents count as Lucene documents).
func (c *Cluster) indexStatsSections(ix *Index) M {
	st := newStatsSections()
	docs, size := 0, int64(0)
	for _, s := range catIndexInfo(ix).Shards {
		docs += s.Docs
		size += s.Bytes
	}
	st["docs"] = M{"count": docs, "deleted": 0}
	st["store"] = M{"size_in_bytes": size, "reserved_in_bytes": 0}
	if indexing, ok := st["indexing"].(M); ok {
		indexing["index_total"] = ix.seqNo + 1
	}
	return st
}

func shardStatsSections(s CatShard) M {
	st := newStatsSections()
	st["docs"] = M{"count": s.Docs, "deleted": 0}
	st["store"] = M{"size_in_bytes": s.Bytes, "reserved_in_bytes": 0}
	if indexing, ok := st["indexing"].(M); ok {
		indexing["index_total"] = s.MaxSeqNo + 1
	}
	return st
}

// addStats sums stats sections (max_* counters keep the maximum).
func addStats(dst, src M) {
	for k, v := range src {
		switch sv := v.(type) {
		case M:
			d, ok := dst[k].(M)
			if !ok {
				d = M{}
				dst[k] = d
			}
			addStats(d, sv)
		default:
			cur, exists := dst[k]
			if !exists {
				dst[k] = cloneDeep(v)
				continue
			}
			a, okA := toFloat(cur)
			b, okB := toFloat(v)
			_, isBool := v.(bool)
			if !okA || !okB || isBool {
				continue
			}
			sum := a + b
			if strings.HasPrefix(k, "max_") {
				sum = math.Max(a, b)
			}
			if n, isNum := v.(json.Number); isNum && strings.Contains(n.String(), ".") {
				dst[k] = json.Number(strconv.FormatFloat(sum, 'f', 1, 64))
			} else {
				dst[k] = int64(sum)
			}
		}
	}
}

// ResolveIndex implements GET /_resolve/index/{name} for concrete indices and
// aliases (IndexAbstractionResolver): missing names are ignored, wildcards
// expand to visible indices in the requested states and to aliases
// regardless of index state.
func (c *Cluster) ResolveIndex(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, err := strictExpandOpenOptions.withParams(p)
	if err != nil {
		return fail(err)
	}
	r := c.newExprResolver()
	replaceWildcards := o.expandOpen || o.expandClosed
	var names []string
	wildcardSeen := false
	for _, expr := range splitCommaJava(expr) {
		name, minus := expr, false
		if expr != "" && expr[0] == '-' && wildcardSeen {
			name, minus = expr[1:], true
		}
		if replaceWildcards && strings.Contains(name, "*") {
			wildcardSeen = true
			var matched []string
			for _, candidate := range r.names {
				if starMatch(name, candidate) && r.visible(name, candidate, o) {
					matched = append(matched, candidate)
				}
			}
			if len(matched) == 0 {
				if !o.allowNoIndices {
					return fail(errIndexNotFound(name))
				}
				continue
			}
			if minus {
				drop := map[string]bool{}
				for _, m := range matched {
					drop[m] = true
				}
				kept := names[:0]
				for _, n := range names {
					if !drop[n] {
						kept = append(kept, n)
					}
				}
				names = kept
			} else {
				names = append(names, matched...)
			}
			continue
		}
		if minus {
			for i, n := range names {
				if n == name {
					names = append(names[:i], names[i+1:]...)
					break
				}
			}
			continue
		}
		names = append(names, name)
	}
	indices := []M{}
	aliases := []M{}
	for _, name := range names {
		ab := r.lookup[name]
		if ab == nil {
			continue
		}
		if ab.alias {
			members := make([]string, 0, len(ab.indices))
			for _, ix := range ab.indices {
				members = append(members, ix.Name)
			}
			sort.Strings(members)
			aliases = append(aliases, M{"name": name, "indices": members})
			continue
		}
		ix := ab.indices[0]
		attrs := []string{"open"}
		if ix.stateClosed {
			attrs = []string{"closed"}
		}
		if ab.hidden {
			attrs = append(attrs, "hidden")
		}
		sort.Strings(attrs)
		entry := M{"name": name, "attributes": attrs}
		if len(ix.Aliases) > 0 {
			aliasNames := make([]string, 0, len(ix.Aliases))
			for a := range ix.Aliases {
				aliasNames = append(aliasNames, a)
			}
			sort.Strings(aliasNames)
			entry["aliases"] = aliasNames
		}
		indices = append(indices, entry)
	}
	sort.SliceStable(indices, func(i, j int) bool { return indices[i]["name"].(string) < indices[j]["name"].(string) })
	sort.SliceStable(aliases, func(i, j int) bool { return aliases[i]["name"].(string) < aliases[j]["name"].(string) })
	outIndices := make([]any, len(indices))
	for i, e := range indices {
		outIndices[i] = e
	}
	outAliases := make([]any, len(aliases))
	for i, e := range aliases {
		outAliases[i] = e
	}
	return ok(M{"indices": outIndices, "aliases": outAliases, "data_streams": []any{}})
}

// visible is IndexAbstractionResolver.isIndexVisible.
func (r *exprResolver) visible(expr, name string, o indicesOptions) bool {
	ab := r.lookup[name]
	implicitHidden := strings.HasPrefix(name, ".") && strings.HasPrefix(expr, ".")
	if ab.alias {
		return !o.ignoreAliases && (!ab.hidden || o.expandHidden || implicitHidden)
	}
	if ab.hidden && !o.expandHidden && !implicitHidden {
		return false
	}
	if ab.indices[0].stateClosed {
		return o.expandClosed
	}
	return o.expandOpen
}

func shards(n int) M {
	return M{"total": n, "successful": n, "skipped": 0, "failed": 0}
}

// Writes target one primary shard. Replicas are unassigned in this
// single-node cluster, but OpenSearch still reports the configured shard-copy
// count in total and only the active primary as successful.
func writeShards(ix *Index) M {
	replicas := 1
	if ix != nil {
		replicas = getInt(getMap(ix.Settings, "index"), "number_of_replicas", replicas)
	}
	if replicas < 0 {
		replicas = 0
	}
	return M{"total": replicas + 1, "successful": 1, "failed": 0}
}

// searchShards reports primary shard groups rather than in-memory index
// objects; one osmem Index can model multiple OpenSearch primary shards.
func searchShards(ts []target) M {
	total := 0
	for _, t := range ts {
		idx := getMap(t.ix.Settings, "index")
		n := getInt(idx, "number_of_shards", 1)
		if n < 1 {
			n = 1
		}
		total += n
	}
	return shards(total)
}

// Acknowledge is the response of no-op broadcast operations (_refresh,
// _flush, ...), which reject closed indices.
func (c *Cluster) Acknowledge(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	indices, err := c.resolveIndices(expr, p, searchIndicesOptions)
	if err != nil {
		return fail(err)
	}
	return ok(M{"_shards": broadcastShards(indices)})
}

// ensureIndex returns the write index for a name, auto-creating it.
func (c *Cluster) ensureIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		if ix.stateClosed {
			return nil, errIndexClosed(ix)
		}
		return c.writable(ix.Name)
	}
	if len(c.aliasTargets(name)) > 0 {
		ix, err := c.writeIndex(name)
		if err != nil {
			return nil, err
		}
		if ix.stateClosed {
			return nil, errIndexClosed(ix)
		}
		return c.writable(ix.Name)
	}
	if t := c.matchingComposableTemplate(name); t != nil && t.DataStream != nil {
		// OpenSearch would create a data stream here
		return nil, errDataStreamsUnsupported()
	}
	if err := validateIndexName(name); err != nil {
		return nil, err
	}
	ix, err := c.buildIndex(name, M{})
	if err != nil {
		return nil, err
	}
	c.indices[name] = ix
	return ix, nil
}

// Info implements GET /.
func (c *Cluster) Info() (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.compatibilityMode() {
		return ok(M{
			"name":         "osmem-node",
			"cluster_name": c.Name,
			"cluster_uuid": "osmem-cluster",
			"version": M{
				"number": "7.10.2", "build_flavor": "default", "build_type": "tar", "build_hash": "osmem",
				"build_date": "2026-01-01T00:00:00.000000000Z", "build_snapshot": false, "lucene_version": "8.7.0",
				"minimum_wire_compatibility_version": "6.8.0", "minimum_index_compatibility_version": "6.0.0-beta1",
			},
			"tagline": "You Know, for Search",
		})
	}
	return ok(M{
		"name":         "osmem-node",
		"cluster_name": c.Name,
		"cluster_uuid": "osmem-cluster",
		"version": M{
			"distribution":                        "opensearch",
			"number":                              Version,
			"build_type":                          "tar",
			"build_hash":                          "osmem",
			"build_date":                          "2026-01-01T00:00:00.000000000Z",
			"build_snapshot":                      false,
			"lucene_version":                      "10.5.0",
			"minimum_wire_compatibility_version":  "2.19.0",
			"minimum_index_compatibility_version": "2.0.0",
		},
		"tagline": "The OpenSearch Project: https://opensearch.org/",
	})
}

// compatibilityMode reports whether compatibility.override_main_response_version
// is set, which makes GET / mimic Elasticsearch 7.10.2 for older clients.
func (c *Cluster) compatibilityMode() bool {
	for _, k := range []string{"persistent", "transient"} {
		if m, ok := c.clusterSettings[k].(M); ok {
			if v, ok := m["compatibility.override_main_response_version"]; ok && getBool(M{"v": v}, "v", false) {
				return true
			}
		}
	}
	return false
}

var healthStatusRank = map[string]int{"green": 0, "yellow": 1, "red": 2}

type healthRequest struct {
	level          string
	waitForStatus  string
	waitForShards  int
	waitForNodes   string
	timeoutMillis  int64
	hasWaitParams  bool
	timeoutDisplay string
}

// parseHealthRequest validates the parameters of GET /_cluster/health.
func parseHealthRequest(p Params) (*healthRequest, error) {
	r := &healthRequest{level: "cluster", waitForShards: activeShardsDefault, timeoutMillis: 30000}
	if v := p.Get("level"); v != "" {
		switch strings.ToUpper(v) {
		case "CLUSTER", "INDICES", "SHARDS", "AWARENESS_ATTRIBUTES":
			r.level = strings.ToLower(v)
		default:
			// OpenSearch ignores unknown levels
		}
	}
	if v := p.Get("wait_for_status"); v != "" {
		upper := strings.ToUpper(v)
		if upper != "GREEN" && upper != "YELLOW" && upper != "RED" {
			return nil, javaIllegalArgument("No enum constant org.opensearch.cluster.health.ClusterHealthStatus." + upper)
		}
		r.waitForStatus = strings.ToLower(v)
		r.hasWaitParams = true
	}
	if p.Has("timeout") {
		ms, err := parseTimeSetting("timeout", p.Get("timeout"))
		if err != nil {
			return nil, err
		}
		r.timeoutMillis = ms
	}
	if p.Has("wait_for_active_shards") {
		n, err := activeShardCountValue(p.Get("wait_for_active_shards"))
		if err != nil {
			return nil, err
		}
		r.waitForShards = n
		r.hasWaitParams = true
	}
	if v := p.Get("wait_for_nodes"); v != "" {
		num := strings.TrimLeft(v, "<>=")
		for _, wrap := range []string{"ge(", "le(", "gt(", "lt("} {
			if strings.HasPrefix(v, wrap) && strings.HasSuffix(v, ")") {
				num = v[len(wrap) : len(v)-1]
			}
		}
		if _, err := strconv.ParseInt(num, 10, 32); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: `For input string: "` + num + `"`}
		}
		r.waitForNodes = v
		r.hasWaitParams = true
	}
	if v := p.Get("wait_for_events"); v != "" {
		if !oneOf(strings.ToUpper(v), "IMMEDIATE", "URGENT", "HIGH", "NORMAL", "LOW", "LANGUID") {
			return nil, javaIllegalArgument("No enum constant org.opensearch.common.Priority." + strings.ToUpper(v))
		}
	}
	return r, nil
}

func nodesConditionMet(expr string) bool {
	nodes := 1
	parse := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case strings.HasPrefix(expr, ">="):
		return nodes >= parse(expr[2:])
	case strings.HasPrefix(expr, "<="):
		return nodes <= parse(expr[2:])
	case strings.HasPrefix(expr, ">"):
		return nodes > parse(expr[1:])
	case strings.HasPrefix(expr, "<"):
		return nodes < parse(expr[1:])
	case strings.HasPrefix(expr, "ge("):
		return nodes >= parse(expr[3:len(expr)-1])
	case strings.HasPrefix(expr, "le("):
		return nodes <= parse(expr[3:len(expr)-1])
	case strings.HasPrefix(expr, "gt("):
		return nodes > parse(expr[3:len(expr)-1])
	case strings.HasPrefix(expr, "lt("):
		return nodes < parse(expr[3:len(expr)-1])
	}
	return nodes == parse(expr)
}

// Health implements GET /_cluster/health[/{index}]. Like OpenSearch it waits
// (up to timeout, default 30s) for the wait_for_* conditions and for
// requested indices that do not exist yet, and answers 408 with timed_out
// when they are not met. Replicas are never assigned on the single node, so
// wait_for_status=green on an index with replicas always times out.
func (c *Cluster) Health(expr string, p Params) (Response, error) {
	req, err := parseHealthRequest(p)
	if err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(time.Duration(req.timeoutMillis) * time.Millisecond)
	for {
		body, satisfied, err := c.healthOnce(expr, p, req)
		if err != nil {
			return fail(err)
		}
		if satisfied {
			return ok(body)
		}
		if !time.Now().Before(deadline) {
			body["timed_out"] = true
			return Response{Status: http.StatusRequestTimeout, Body: body}, nil
		}
		wait := time.Until(deadline)
		if wait > 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
		time.Sleep(wait)
	}
}

func (c *Cluster) healthOnce(expr string, p Params, req *healthRequest) (M, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var indices []*Index
	missing := false
	if expr == "" || expr == "_all" || expr == "*" {
		for _, name := range c.sortedIndexNames() {
			indices = append(indices, c.indices[name])
		}
	} else {
		resolved, err := c.resolveIndices(expr, p, lenientExpandHiddenOptions)
		if err != nil {
			return nil, false, err
		}
		indices = resolved
		for _, item := range splitList(expr) {
			if strings.HasPrefix(item, "-") || strings.ContainsAny(item, "*?") {
				continue
			}
			if _, isIndex := c.indices[item]; !isIndex && len(c.aliasTargets(item)) == 0 {
				missing = true
			}
		}
	}
	activePrimary, active, unassigned, copies := 0, 0, 0, 0
	indicesOut := M{}
	status := "green"
	for _, ix := range indices {
		shards, replicas := indexShardCount(ix), indexReplicaCount(ix)
		copies += shards * (replicas + 1)
		// every copy of a closed index is unassigned and turns the status red
		indexStatus, activeCopies, unassignedCopies, activePerShard := "green", shards, shards*replicas, 1
		if ix.stateClosed {
			indexStatus, activeCopies, unassignedCopies, activePerShard = "red", 0, shards*(replicas+1), 0
		} else if replicas > 0 {
			indexStatus = "yellow"
		}
		activePrimary += activeCopies
		active += activeCopies
		unassigned += unassignedCopies
		if healthStatusRank[indexStatus] > healthStatusRank[status] {
			status = indexStatus
		}
		if req.level == "indices" || req.level == "shards" {
			entry := M{"status": indexStatus, "number_of_shards": shards, "number_of_replicas": replicas, "active_primary_shards": activeCopies,
				"active_shards": activeCopies, "relocating_shards": 0, "initializing_shards": 0, "unassigned_shards": unassignedCopies}
			if req.level == "shards" {
				shardsOut := M{}
				for s := 0; s < shards; s++ {
					shardsOut[strconv.Itoa(s)] = M{"status": indexStatus, "primary_active": activePerShard == 1, "active_shards": activePerShard, "relocating_shards": 0,
						"initializing_shards": 0, "unassigned_shards": replicas + 1 - activePerShard}
				}
				entry["shards"] = shardsOut
			}
			indicesOut[ix.Name] = entry
		}
	}
	// the percentage is 100 when the requested indices are green and computed
	// over every shard of the cluster otherwise; a missing index makes the
	// status red afterwards
	percent := 100.0
	if status != "green" {
		clusterActive, clusterCopies := 0, 0
		for _, ix := range c.indices {
			clusterCopies += indexShardCount(ix) * (indexReplicaCount(ix) + 1)
			if !ix.stateClosed {
				clusterActive += indexShardCount(ix)
			}
		}
		if clusterCopies > 0 {
			percent = float64(clusterActive) / float64(clusterCopies) * 100
		}
	}
	if missing {
		status = "red"
	}
	body := M{
		"cluster_name": c.Name, "status": status, "timed_out": false, "number_of_nodes": 1, "number_of_data_nodes": 1,
		"discovered_master": true, "discovered_cluster_manager": true, "active_primary_shards": activePrimary, "active_shards": active,
		"relocating_shards": 0, "initializing_shards": 0, "unassigned_shards": unassigned, "delayed_unassigned_shards": 0,
		"number_of_pending_tasks": 0, "number_of_in_flight_fetch": 0, "task_max_waiting_in_queue_millis": 0,
		"active_shards_percent_as_number": Double(percent),
	}
	if req.level == "indices" || req.level == "shards" {
		body["indices"] = indicesOut
	}
	satisfied := !missing
	if req.waitForStatus != "" && healthStatusRank[status] > healthStatusRank[req.waitForStatus] {
		satisfied = false
	}
	switch {
	case req.waitForShards == activeShardsAll:
		if active < copies {
			satisfied = false
		}
	case req.waitForShards > 0:
		if active < req.waitForShards {
			satisfied = false
		}
	}
	if req.waitForNodes != "" && !nodesConditionMet(req.waitForNodes) {
		satisfied = false
	}
	return body, satisfied, nil
}

// ClusterSettings implements GET /_cluster/settings (flat_settings,
// include_defaults).
func (c *Cluster) ClusterSettings(p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	flatOut := p.Bool("flat_settings", false)
	out := M{}
	set := map[string]bool{}
	for _, scope := range []string{"persistent", "transient"} {
		flat := map[string]any{}
		for k, v := range c.clusterSettings[scope].(M) {
			flat[k] = v
			set[k] = true
		}
		out[scope] = renderSettings(flat, flatOut)
	}
	if p.Bool("include_defaults", false) {
		out["defaults"] = renderSettings(clusterSettingDefaults(set), flatOut)
	}
	return ok(out)
}

// well known node settings that exist but cannot be updated at runtime
var staticClusterSettings = map[string]bool{
	"cluster.name": true, "node.name": true, "node.roles": true, "path.data": true, "path.logs": true, "path.repo": true,
	"network.host": true, "http.port": true, "transport.port": true, "discovery.type": true, "discovery.seed_hosts": true,
	"cluster.initial_cluster_manager_nodes": true, "cluster.initial_master_nodes": true, "bootstrap.memory_lock": true,
	"http.cors.enabled": true, "http.max_content_length": true, "node.attr": true,
}

// PutClusterSettings implements PUT /_cluster/settings with its URL
// parameters. Only registered dynamic settings are accepted; the response
// echoes the settings the request applied.
func (c *Cluster) PutClusterSettings(body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	updates := map[string]map[string]any{}
	for _, scope := range []string{"persistent", "transient"} {
		raw, present := body[scope]
		if !present || raw == nil {
			continue
		}
		m, isObject := raw.(M)
		if !isObject {
			return fail(&Error{Status: http.StatusInternalServerError, Type: "class_cast_exception", Reason: "class " + adminJavaClassName(raw) + " cannot be cast to class java.util.Map (" + adminJavaClassName(raw) + " and java.util.Map are in module java.base of loader 'bootstrap')"})
		}
		flat := map[string]any{}
		flattenSettingsBody(flat, "", m)
		updates[scope] = flat
	}
	if len(updates["persistent"]) == 0 && len(updates["transient"]) == 0 {
		return fail(errActionRequestValidation("no settings to update"))
	}
	onlyBlocks := true
	for _, scope := range []string{"persistent", "transient"} {
		for key := range updates[scope] {
			if key != "cluster.blocks.read_only" && key != "cluster.blocks.read_only_allow_delete" {
				onlyBlocks = false
			}
		}
	}
	if !onlyBlocks {
		if err := c.checkGlobalBlock(blockMetadataWrite); err != nil {
			return fail(err)
		}
	}
	// transient settings are applied (and validated) before persistent ones
	for _, scope := range []string{"transient", "persistent"} {
		for _, key := range sortedFlatKeys(updates[scope]) {
			value := updates[scope][key]
			if value == nil && strings.HasSuffix(key, "*") {
				continue
			}
			def, known := lookupClusterSetting(key)
			if !known {
				if staticClusterSettings[key] && value != nil {
					return fail(errSettings(scope + " setting [" + key + "], not dynamically updateable"))
				}
				return fail(errSettings(scope + " setting [" + key + "], not recognized"))
			}
			if err := validateSetting(key, def, value); err != nil {
				return fail(err)
			}
		}
	}
	flatOut := p.Bool("flat_settings", false)
	out := M{"acknowledged": true}
	for _, scope := range []string{"persistent", "transient"} {
		dst := c.clusterSettings[scope].(M)
		applied := map[string]any{}
		for _, key := range sortedFlatKeys(updates[scope]) {
			value := updates[scope][key]
			if value == nil {
				for k := range dst {
					if simpleMatch(key, k) {
						delete(dst, k)
					}
				}
				continue
			}
			dst[key] = value
			applied[key] = value
		}
		out[scope] = renderSettings(applied, flatOut)
	}
	return ok(out)
}

func adminJavaClassName(v any) string {
	switch v.(type) {
	case string:
		return "java.lang.String"
	case []any:
		return "java.util.ArrayList"
	case bool:
		return "java.lang.Boolean"
	case json.Number, float64:
		return "java.lang.Integer"
	}
	return "java.lang.Object"
}

// cluster blocks ---------------------------------------------------------------

// block levels (ClusterBlockLevel)
const (
	blockRead          = "read"
	blockWrite         = "write"
	blockMetadataRead  = "metadata_read"
	blockMetadataWrite = "metadata_write"
)

type clusterBlock struct {
	setting     string
	id          int
	description string
	status      int
	levels      []string
}

func (b clusterBlock) String() string {
	return fmt.Sprintf("%s/%d/%s", strings.ToUpper(strings.ReplaceAll(http.StatusText(b.status), " ", "_")), b.id, b.description)
}

var indexBlockDefs = []clusterBlock{
	{"index.blocks.read_only", 5, "index read-only (api)", http.StatusForbidden, []string{blockWrite, blockMetadataWrite}},
	{"index.blocks.read", 7, "index read (api)", http.StatusForbidden, []string{blockRead}},
	{"index.blocks.write", 8, "index write (api)", http.StatusForbidden, []string{blockWrite}},
	{"index.blocks.metadata", 9, "index metadata (api)", http.StatusForbidden, []string{blockMetadataRead, blockMetadataWrite}},
	{"index.blocks.read_only_allow_delete", 12, "disk usage exceeded flood-stage watermark, index has read-only-allow-delete block", http.StatusTooManyRequests, []string{blockWrite, blockMetadataWrite}},
}

var globalBlockDefs = []clusterBlock{
	{"cluster.blocks.read_only", 6, "cluster read-only (api)", http.StatusForbidden, []string{blockWrite, blockMetadataWrite}},
	{"cluster.blocks.read_only_allow_delete", 13, "cluster read-only / allow delete (api)", http.StatusForbidden, []string{blockWrite, blockMetadataWrite}},
}

func blockHasLevel(b clusterBlock, level string) bool {
	for _, l := range b.levels {
		if l == level {
			return true
		}
	}
	return false
}

// activeIndexBlocks lists the blocks of an index for an operation level.
func activeIndexBlocks(ix *Index, level string) []clusterBlock {
	var out []clusterBlock
	for _, b := range indexBlockDefs {
		if !blockHasLevel(b, level) {
			continue
		}
		if v, ok := getNested(ix.Settings, b.setting); ok && settingString(v) == "true" {
			out = append(out, b)
		}
	}
	return out
}

func clusterBlockError(blocksByIndex map[string][]clusterBlock, order []string, global []clusterBlock) *Error {
	status := 0
	var sb strings.Builder
	describe := func(blocks []clusterBlock) string {
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = b.String()
			if b.status > status {
				status = b.status
			}
		}
		return strings.Join(parts, ", ")
	}
	if len(global) > 0 {
		sb.WriteString("blocked by: [" + describe(global) + "];")
	}
	for _, name := range order {
		sb.WriteString("index [" + name + "] blocked by: [" + describe(blocksByIndex[name]) + "];")
	}
	return &Error{Status: status, Type: "cluster_block_exception", Reason: sb.String()}
}

// checkGlobalBlock returns the cluster_block_exception of the cluster wide
// blocks (cluster.blocks.*) for an operation level, or nil.
func (c *Cluster) checkGlobalBlock(level string) error {
	var blocks []clusterBlock
	for _, b := range globalBlockDefs {
		if !blockHasLevel(b, level) {
			continue
		}
		for _, scope := range []string{"transient", "persistent"} {
			if v, ok := c.clusterSettings[scope].(M)[b.setting]; ok {
				if settingString(v) == "true" {
					blocks = append(blocks, b)
				}
				break
			}
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return clusterBlockError(nil, nil, blocks)
}

// checkBlock returns the cluster_block_exception raised when an operation of
// the given level (blockRead, blockWrite, blockMetadataRead,
// blockMetadataWrite) targets an index that has index.blocks.* set (or the
// cluster has a global block), or nil. Document writes use blockWrite,
// searches and gets blockRead, mapping/settings reads blockMetadataRead and
// mapping/settings/alias changes blockMetadataWrite.
func (c *Cluster) checkBlock(ix *Index, level string) error {
	return c.checkBlocks([]*Index{ix}, level)
}

// checkBlocks is checkBlock for several indices; the exception lists every
// blocked index.
func (c *Cluster) checkBlocks(indices []*Index, level string) error {
	if err := c.checkGlobalBlock(level); err != nil {
		return err
	}
	byIndex := map[string][]clusterBlock{}
	var order []string
	for _, ix := range indices {
		if ix == nil {
			continue
		}
		if blocks := activeIndexBlocks(ix, level); len(blocks) > 0 {
			if _, seen := byIndex[ix.Name]; !seen {
				order = append(order, ix.Name)
			}
			byIndex[ix.Name] = blocks
		}
	}
	if len(order) == 0 {
		return nil
	}
	return clusterBlockError(byIndex, order, nil)
}

// checkDeleteBlock is the block check of index deletion: blocks that do not
// allow releasing resources (read_only, metadata) prevent it.
func (c *Cluster) checkDeleteBlock(indices []*Index) error {
	if err := c.checkGlobalBlock(blockMetadataWrite); err != nil {
		for _, b := range globalBlockDefs {
			if b.setting == "cluster.blocks.read_only" {
				if v, ok := c.clusterSettings["transient"].(M)[b.setting]; ok && settingString(v) == "true" {
					return err
				}
				if v, ok := c.clusterSettings["persistent"].(M)[b.setting]; ok && settingString(v) == "true" {
					return err
				}
			}
		}
	}
	byIndex := map[string][]clusterBlock{}
	var order []string
	for _, ix := range indices {
		var blocks []clusterBlock
		for _, b := range activeIndexBlocks(ix, blockMetadataWrite) {
			if b.setting != "index.blocks.read_only_allow_delete" {
				blocks = append(blocks, b)
			}
		}
		if len(blocks) > 0 {
			order = append(order, ix.Name)
			byIndex[ix.Name] = blocks
		}
	}
	if len(order) == 0 {
		return nil
	}
	return clusterBlockError(byIndex, order, nil)
}

// checkWaitForActiveShards is the check of document writes against the
// requested (or index.write.wait_for_active_shards) number of active shard
// copies: the single node only ever has the primary active, so a request
// needing more copies fails with unavailable_shards_exception. OpenSearch
// fails only after waiting for the request timeout (default 1m); callers
// pass the timeout parameter as given (empty for the default).
func (c *Cluster) checkWaitForActiveShards(ix *Index, requested, timeout string) error {
	count, err := activeShardCountValue(requested)
	if err != nil {
		return err
	}
	if count == activeShardsDefault {
		if v, ok := getNested(ix.Settings, "index.write.wait_for_active_shards"); ok {
			count, _ = activeShardCountValue(settingString(v))
		}
	}
	copies := indexReplicaCount(ix) + 1
	needed := count
	label := strconv.Itoa(count)
	if count == activeShardsAll {
		needed, label = copies, "ALL"
	}
	if count == activeShardsDefault || needed <= 1 {
		return nil
	}
	if timeout == "" {
		timeout = "1m"
	}
	return &Error{Status: http.StatusServiceUnavailable, Type: "unavailable_shards_exception",
		Reason: fmt.Sprintf("[%s][0] Not enough active copies to meet shard count of [%s] (have 1, needed %d). Timeout: [%s]", ix.Name, label, needed, timeout)}
}

// Indices returns the sorted index names (for the public API).
func (c *Cluster) Indices() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sortedIndexNames()
}

// Warnings returns analysis warnings of an index.
func (c *Cluster) Warnings(index string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if ix, ok := c.indices[index]; ok {
		return append([]string(nil), ix.analysis.warnings...)
	}
	return nil
}
