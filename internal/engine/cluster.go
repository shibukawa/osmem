package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version reported by the root endpoint.
const Version = "2.19.0"

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
	closed          bool

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
		indices:         map[string]*Index{},
		templates:       map[string]*Template{},
		legacyTemplates: map[string]*Template{},
		scrolls:         map[string]*scrollState{},
		pits:            map[string]*pitState{},
		clusterSettings: M{"persistent": M{}, "transient": M{}},
		Now:             time.Now,
		Name:            "osmem",
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
	alias  string
}

type resolveOptions struct {
	ignoreUnavailable bool
	allowNoIndices    bool
	allowAliases      bool
}

func resolveOpts(p Params) resolveOptions {
	return resolveOptions{
		ignoreUnavailable: p.Bool("ignore_unavailable", false),
		allowNoIndices:    p.Bool("allow_no_indices", true),
		allowAliases:      true,
	}
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
			out = append(out, target{ix: ix, filter: a.Filter, alias: alias})
		}
	}
	return out
}

func (c *Cluster) aliasNames() []string {
	set := map[string]bool{}
	for _, ix := range c.indices {
		for a := range ix.Aliases {
			set[a] = true
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// resolve expands an index expression (names, aliases, wildcards, _all,
// comma lists, -exclusions) into concrete targets.
func (c *Cluster) resolve(expr string, opts resolveOptions) ([]target, error) {
	if expr == "" || expr == "_all" || expr == "*" {
		var out []target
		for _, name := range c.sortedIndexNames() {
			out = append(out, target{ix: c.indices[name]})
		}
		return out, nil
	}
	var out []target
	seen := map[string]bool{}
	add := func(t target) {
		key := t.ix.Name + "|" + t.alias
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, t)
	}
	items := splitList(expr)
	hasWildcard := false
	for _, item := range items {
		if strings.HasPrefix(item, "-") && len(out) > 0 {
			pat := item[1:]
			filtered := out[:0]
			for _, t := range out {
				if !wildcardMatch(pat, t.ix.Name) && !wildcardMatch(pat, t.alias) {
					filtered = append(filtered, t)
				}
			}
			out = filtered
			continue
		}
		if item == "_all" || item == "*" {
			for _, name := range c.sortedIndexNames() {
				add(target{ix: c.indices[name]})
			}
			hasWildcard = true
			continue
		}
		if strings.ContainsAny(item, "*?") {
			hasWildcard = true
			for _, name := range c.sortedIndexNames() {
				if wildcardMatch(item, name) {
					add(target{ix: c.indices[name]})
				}
			}
			if opts.allowAliases {
				for _, a := range c.aliasNames() {
					if wildcardMatch(item, a) {
						for _, t := range c.aliasTargets(a) {
							add(t)
						}
					}
				}
			}
			continue
		}
		if ix, ok := c.indices[item]; ok {
			add(target{ix: ix})
			continue
		}
		if opts.allowAliases {
			if ts := c.aliasTargets(item); len(ts) > 0 {
				for _, t := range ts {
					add(t)
				}
				continue
			}
		}
		if opts.ignoreUnavailable {
			continue
		}
		return nil, errIndexNotFound(item)
	}
	if len(out) == 0 && hasWildcard && !opts.allowNoIndices {
		return nil, errIndexNotFound(expr)
	}
	return out, nil
}

// resolveOne resolves an expression that must name exactly one index (or an
// alias pointing at one index / with a write index).
func (c *Cluster) resolveWriteIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		return ix, nil
	}
	ts := c.aliasTargets(name)
	switch len(ts) {
	case 0:
		return nil, errIndexNotFound(name)
	case 1:
		return ts[0].ix, nil
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateIndexName(name); err != nil {
		return fail(err)
	}
	if _, ok := c.indices[name]; ok {
		return fail(errIndexExists(name))
	}
	if len(c.aliasTargets(name)) > 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + name + "], already exists as alias", Index: name})
	}
	ix, err := c.buildIndex(name, body)
	if err != nil {
		return fail(err)
	}
	c.indices[name] = ix
	return ok(M{"acknowledged": true, "shards_acknowledged": true, "index": name})
}

// buildIndex creates an index from a create-index body, applying templates.
func (c *Cluster) buildIndex(name string, body M) (*Index, error) {
	settings := M{}
	mappings := M{}
	aliases := M{}
	// legacy templates apply in order, then one composable template
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
		deepMerge(aliases, t.Aliases)
	}
	var best *Template
	for _, t := range c.templates {
		if t.matches(name) && (best == nil || t.Priority > best.Priority) {
			best = t
		}
	}
	if best != nil {
		deepMerge(settings, best.Settings)
		deepMerge(mappings, best.Mappings)
		deepMerge(aliases, best.Aliases)
	}
	if s, ok := body["settings"].(M); ok {
		deepMerge(settings, normalizeSettings(s))
	}
	if m, ok := body["mappings"].(M); ok {
		if doc, ok := m["_doc"].(M); ok && len(m) == 1 {
			m = doc
		}
		deepMerge(mappings, m)
	}
	if a, ok := body["aliases"].(M); ok {
		deepMerge(aliases, a)
	}
	for k := range body {
		switch k {
		case "settings", "mappings", "aliases":
		default:
			return nil, errParsing("unknown key [%s] for create index", k)
		}
	}
	settings = normalizeSettings(settings)
	idx := settings["index"].(M)
	if _, ok := idx["number_of_shards"]; !ok {
		idx["number_of_shards"] = "1"
	}
	if _, ok := idx["number_of_replicas"]; !ok {
		idx["number_of_replicas"] = "1"
	}
	mp, err := parseMapping(mappings)
	if err != nil {
		return nil, err
	}
	if err := validateMappingLimits(mp, settings); err != nil {
		return nil, err
	}
	ix, err := newIndex(name, settings, mp, c.now(), c.warnFunc())
	if err != nil {
		return nil, err
	}
	idx["uuid"] = ix.UUID
	idx["creation_date"] = strconv.FormatInt(ix.Created.UnixMilli(), 10)
	idx["provided_name"] = name
	idx["version"] = M{"created": "136427827"}
	for aname, araw := range aliases {
		spec, _ := araw.(M)
		a, err := parseAlias(spec)
		if err != nil {
			return nil, err
		}
		ix.Aliases[aname] = a
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

// DeleteIndex implements DELETE /{index}.
func (c *Cluster) DeleteIndex(expr string, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	opts := resolveOpts(p)
	opts.allowAliases = false
	for _, item := range splitList(expr) {
		if _, isIndex := c.indices[item]; !isIndex && len(c.aliasTargets(item)) > 0 {
			return fail(errIllegalArgument("The provided expression [%s] matches an alias, specify the corresponding concrete indices instead.", item))
		}
	}
	ts, err := c.resolve(expr, opts)
	if err != nil {
		return fail(err)
	}
	if len(ts) == 0 && !strings.ContainsAny(expr, "*?") && expr != "_all" {
		return fail(errIndexNotFound(expr))
	}
	for _, t := range ts {
		if _, ok := c.indices[t.ix.Name]; ok {
			t.ix.release()
			delete(c.indices, t.ix.Name)
		}
	}
	return ok(M{"acknowledged": true})
}

// IndexExists implements HEAD /{index}.
func (c *Cluster) IndexExists(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil || len(ts) == 0 {
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

// GetIndex implements GET /{index}.
func (c *Cluster) GetIndex(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		out[t.ix.Name] = M{
			"aliases":  aliasesJSON(t.ix),
			"mappings": t.ix.Mapping.toJSON(),
			"settings": settingsJSON(t.ix, p),
		}
	}
	return ok(out)
}

func aliasesJSON(ix *Index) M {
	out := M{}
	for name, a := range ix.Aliases {
		out[name] = a.toJSON()
	}
	return out
}

func settingsJSON(ix *Index, p Params) M {
	s := cloneDeep(ix.Settings).(M)
	if filter := p.Get("settings_filter"); filter != "" {
		flat := M{}
		flattenInto(flat, "", s)
		selected := M{}
		for key, value := range flat {
			for _, pattern := range splitList(filter) {
				matched, _ := path.Match(pattern, key)
				if matched || key == pattern || strings.HasPrefix(key, pattern+".") {
					selected[key] = value
					break
				}
			}
		}
		if p.Bool("flat_settings", false) {
			return selected
		}
		nested := M{}
		for key, value := range selected {
			setNested(nested, key, value)
		}
		return nested
	}
	if p.Bool("flat_settings", false) {
		flat := M{}
		flattenInto(flat, "", s)
		return flat
	}
	return s
}

func flattenInto(dst M, prefix string, m M) {
	for k, v := range m {
		if sub, ok := v.(M); ok {
			flattenInto(dst, prefix+k+".", sub)
			continue
		}
		dst[prefix+k] = v
	}
}

// GetMapping implements GET /{index}/_mapping.
func (c *Cluster) GetMapping(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		out[t.ix.Name] = M{"mappings": t.ix.Mapping.toJSON()}
	}
	return ok(out)
}

// GetFieldMapping implements GET /{index}/_mapping/field/{fields}.
func (c *Cluster) GetFieldMapping(expr, fields string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		fm := M{}
		for _, pat := range splitList(fields) {
			for _, path := range t.ix.Mapping.leafFields(pat) {
				f, _, ok := t.ix.Mapping.resolve(path)
				if !ok {
					continue
				}
				leaf := path[strings.LastIndex(path, ".")+1:]
				fm[path] = M{"full_name": path, "mapping": M{leaf: f.toJSON()}}
			}
		}
		out[t.ix.Name] = M{"mappings": fm}
	}
	return ok(out)
}

// PutMapping implements PUT /{index}/_mapping.
func (c *Cluster) PutMapping(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	if len(ts) == 0 {
		return fail(errIndexNotFound(expr))
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
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	out := M{}
	for _, t := range ts {
		out[t.ix.Name] = M{"settings": settingsJSON(t.ix, p)}
	}
	return ok(out)
}

// PutSettings implements PUT /{index}/_settings.
func (c *Cluster) PutSettings(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	upd := normalizeSettings(body)
	idx := upd["index"].(M)
	for k := range idx {
		switch k {
		case "number_of_shards", "uuid", "creation_date", "provided_name", "version":
			return fail(errIllegalArgument("Can't update non dynamic settings [[index.%s]] for open indices", k))
		}
	}
	for _, t := range ts {
		ix, err := c.writable(t.ix.Name)
		if err != nil {
			return fail(err)
		}
		if _, ok := idx["analysis"]; ok {
			return fail(errIllegalArgument("Can't update non dynamic settings [[index.analysis]] for open indices"))
		}
		if p.Bool("preserve_existing", false) {
			deepMergeMissing(ix.Settings, upd)
		} else {
			deepMerge(ix.Settings, upd)
		}
	}
	return ok(M{"acknowledged": true})
}

func deepMergeMissing(dst, src M) {
	for k, value := range src {
		incoming, nested := value.(M)
		if !nested {
			if _, exists := dst[k]; !exists {
				dst[k] = value
			}
			continue
		}
		current, exists := dst[k].(M)
		if !exists {
			current = M{}
			dst[k] = current
		}
		deepMergeMissing(current, incoming)
	}
}

// IndexStats implements GET /{index}/_stats (documents only).
func (c *Cluster) IndexStats(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	total := 0
	indices := M{}
	metric := p.Get("metric")
	filterMetrics := metric != "" && metric != "_all" && metric != "all"
	for _, t := range ts {
		n := t.ix.DocCount()
		total += n
		st := M{"docs": M{"count": n, "deleted": 0}, "store": M{"size_in_bytes": 0}}
		if filterMetrics {
			st = selectStatsMetrics(st, metric)
		}
		indices[t.ix.Name] = M{"uuid": t.ix.UUID, "primaries": st, "total": st}
	}
	st := M{"docs": M{"count": total, "deleted": 0}, "store": M{"size_in_bytes": 0}}
	if filterMetrics {
		st = selectStatsMetrics(st, metric)
	}
	return ok(M{"_shards": searchShards(ts), "_all": M{"primaries": st, "total": st}, "indices": indices})
}

// ResolveIndex implements GET /_resolve/index/{name} for concrete indices and
// aliases. Data streams and closed indices are not modeled by osmem.
func (c *Cluster) ResolveIndex(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	includeHidden, expandOpen, expandClosed, acceptWildcards := false, true, false, true
	if p.Has("expand_wildcards") {
		values := splitList(p.Get("expand_wildcards"))
		includeHidden, expandOpen, expandClosed = false, false, false
		if len(values) == 0 {
			return fail(errIllegalArgument("[expand_wildcards] must contain at least one value"))
		}
		for _, value := range values {
			switch value {
			case "all":
				includeHidden, expandOpen, expandClosed = true, true, true
			case "open":
				expandOpen = true
			case "hidden":
				includeHidden = true
			case "closed":
				expandClosed = true
			case "none":
				acceptWildcards = false
			default:
				return fail(errIllegalArgument("[expand_wildcards] parameter must be one of [all, open, closed, hidden, none], found [%s]", value))
			}
		}
		// The API example uses `hidden` by itself; treat it as open+hidden.
		if includeHidden && !expandOpen && !expandClosed {
			expandOpen = true
		}
	}
	items := splitList(expr)
	for _, item := range items {
		if isWildcardIndexExpression(item) && !acceptWildcards {
			return fail(errIllegalArgument("wildcard expressions are not accepted when [expand_wildcards] is [none]"))
		}
	}
	targets, err := c.resolve(expr, resolveOptions{allowAliases: true, allowNoIndices: true})
	if err != nil {
		return fail(err)
	}
	indexSet := map[string]*Index{}
	for _, target := range targets {
		ix := target.ix
		if ix == nil {
			continue
		}
		hidden := getBool(getMap(ix.Settings, "index"), "hidden", false)
		direct, wildcardMatchTarget := false, false
		for _, item := range items {
			if !isWildcardIndexExpression(item) {
				if item == ix.Name || (target.alias != "" && item == target.alias) {
					direct = true
				}
				continue
			}
			if item == "*" || item == "_all" || wildcardMatch(item, ix.Name) || (target.alias != "" && wildcardMatch(item, target.alias)) {
				wildcardMatchTarget = true
			}
		}
		// expand_wildcards filters wildcard expansion. Concrete index and alias
		// names remain resolvable regardless of these wildcard-only options.
		if !direct && wildcardMatchTarget && (!expandOpen || (hidden && !includeHidden)) {
			continue
		}
		if direct || wildcardMatchTarget {
			indexSet[ix.Name] = ix
		}
	}
	requestedAliases := map[string]bool{}
	for _, alias := range c.aliasNames() {
		for _, item := range items {
			if item == alias || (isWildcardIndexExpression(item) && (item == "*" || item == "_all" || wildcardMatch(item, alias))) {
				requestedAliases[alias] = true
			}
		}
	}
	aliasIndices := map[string][]string{}
	indexAliases := map[string][]string{}
	for alias := range requestedAliases {
		for _, target := range c.aliasTargets(alias) {
			if _, resolved := indexSet[target.ix.Name]; !resolved {
				continue
			}
			aliasIndices[alias] = append(aliasIndices[alias], target.ix.Name)
			indexAliases[target.ix.Name] = append(indexAliases[target.ix.Name], alias)
		}
		sort.Strings(aliasIndices[alias])
	}
	indices := make([]any, 0, len(indexSet))
	indexNames := make([]string, 0, len(indexSet))
	for name := range indexSet {
		indexNames = append(indexNames, name)
	}
	sort.Strings(indexNames)
	for _, name := range indexNames {
		aliases := indexAliases[name]
		attrs := []string{"open"}
		entry := M{"name": name, "attributes": attrs}
		if len(aliases) > 0 {
			entry["aliases"] = aliases
		}
		indices = append(indices, entry)
	}
	aliases := make([]any, 0, len(aliasIndices))
	aliasNames := make([]string, 0, len(aliasIndices))
	for name := range aliasIndices {
		if len(aliasIndices[name]) == 0 {
			continue
		}
		aliasNames = append(aliasNames, name)
	}
	sort.Strings(aliasNames)
	for _, name := range aliasNames {
		aliases = append(aliases, M{"name": name, "indices": aliasIndices[name]})
	}
	return ok(M{"indices": indices, "aliases": aliases, "data_streams": []any{}})
}

func isWildcardIndexExpression(expr string) bool {
	return expr == "*" || expr == "_all" || strings.ContainsAny(expr, "*?")
}

func selectStatsMetrics(stats M, metric string) M {
	selected := M{}
	for _, name := range splitList(metric) {
		if value, ok := stats[name]; ok {
			selected[name] = value
		}
	}
	return selected
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

// Acknowledge is the response of no-op index operations (_refresh, _flush, ...).
func (c *Cluster) Acknowledge(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if expr != "" {
		if _, err := c.resolve(expr, resolveOpts(p)); err != nil {
			return fail(err)
		}
	}
	return ok(M{"_shards": shards(1)})
}

// Acknowledged returns {"acknowledged": true} after validating the target.
func (c *Cluster) Acknowledged(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if expr != "" {
		if _, err := c.resolve(expr, resolveOpts(p)); err != nil {
			return fail(err)
		}
	}
	return ok(M{"acknowledged": true, "shards_acknowledged": true})
}

// ensureIndex returns the write index for a name, auto-creating it.
func (c *Cluster) ensureIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		return c.writable(ix.Name)
	}
	if ix, err := c.resolveWriteIndex(name); err == nil {
		return c.writable(ix.Name)
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
			"lucene_version":                      "9.12.1",
			"minimum_wire_compatibility_version":  "7.10.0",
			"minimum_index_compatibility_version": "7.0.0",
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

// Health implements GET /_cluster/health.
func (c *Cluster) Health(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	indices := make([]*Index, 0, len(c.indices))
	if expr != "" {
		ts, err := c.resolve(expr, resolveOpts(p))
		if err != nil {
			return fail(err)
		}
		for _, target := range ts {
			indices = append(indices, target.ix)
		}
	} else {
		for _, ix := range c.indices {
			indices = append(indices, ix)
		}
	}
	activePrimary := 0
	unassigned := 0
	totalShards := 0
	for _, ix := range indices {
		settings := getMap(ix.Settings, "index")
		primaries := getInt(settings, "number_of_shards", 1)
		replicas := getInt(settings, "number_of_replicas", 1)
		activePrimary += primaries
		unassigned += primaries * replicas
		totalShards += primaries * (replicas + 1)
	}
	activeShards := activePrimary // this model has one data node, so replica copies stay unassigned.
	status := "green"
	if unassigned > 0 {
		status = "yellow"
	}
	activePercent := 100.0
	if totalShards > 0 {
		activePercent = float64(activeShards) * 100 / float64(totalShards)
	}
	return ok(M{
		"cluster_name": c.Name, "status": status, "timed_out": false, "number_of_nodes": 1, "number_of_data_nodes": 1,
		"discovered_master": true, "discovered_cluster_manager": true, "active_primary_shards": activePrimary, "active_shards": activeShards,
		"relocating_shards": 0, "initializing_shards": 0, "unassigned_shards": unassigned, "delayed_unassigned_shards": 0,
		"number_of_pending_tasks": 0, "number_of_in_flight_fetch": 0, "task_max_waiting_in_queue_millis": 0,
		"active_shards_percent_as_number": activePercent,
	})
}

// ClusterSettings implements GET /_cluster/settings.
func (c *Cluster) ClusterSettings() (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return ok(cloneDeep(c.clusterSettings))
}

// PutClusterSettings implements PUT /_cluster/settings.
func (c *Cluster) PutClusterSettings(body M) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range []string{"persistent", "transient"} {
		if v, ok := body[k].(M); ok {
			flat := M{}
			flattenInto(flat, "", v)
			dst := c.clusterSettings[k].(M)
			for fk, fv := range flat {
				if fv == nil {
					delete(dst, fk)
				} else {
					dst[fk] = fv
				}
			}
		}
	}
	out := M{"acknowledged": true}
	for _, k := range []string{"persistent", "transient"} {
		nested := M{}
		for fk, fv := range c.clusterSettings[k].(M) {
			setNested(nested, fk, fv)
		}
		out[k] = nested
	}
	return ok(out)
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
