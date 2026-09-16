package engine

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Index expression resolution, modeled on OpenSearch's
// IndexNameExpressionResolver: wildcard expansion honours the index state
// (open/closed), index.hidden and hidden aliases, "-name" excludes only after
// a wildcard, and every concrete index is reached once. Search-like requests
// additionally compute the alias filters of each index from the expression.

// indicesOptions mirrors IndicesOptions.
type indicesOptions struct {
	ignoreUnavailable bool
	allowNoIndices    bool
	expandOpen        bool
	expandClosed      bool
	expandHidden      bool
	forbidClosed      bool
	ignoreAliases     bool
}

var (
	// strictExpandOpenAndForbidClosed: search, count, stats, refresh...
	searchIndicesOptions = indicesOptions{allowNoIndices: true, expandOpen: true, forbidClosed: true}
	// strictExpandOpen: get index, mappings, settings, close, field caps
	strictExpandOpenOptions = indicesOptions{allowNoIndices: true, expandOpen: true}
	// open and closed indices: _cat/indices, index exists
	strictExpandOptions = indicesOptions{allowNoIndices: true, expandOpen: true, expandClosed: true}
	// strictExpandHidden: get aliases
	strictExpandHiddenOptions = indicesOptions{allowNoIndices: true, expandOpen: true, expandClosed: true, expandHidden: true}
	// lenientExpandOpen: the alias filter lookup of searches
	lenientExpandOpenOptions = indicesOptions{ignoreUnavailable: true, allowNoIndices: true, expandOpen: true}
	// lenientExpandHidden: cluster health
	lenientExpandHiddenOptions = indicesOptions{ignoreUnavailable: true, allowNoIndices: true, expandOpen: true, expandClosed: true, expandHidden: true}
	// _open expands to closed indices only
	openIndexOptions = indicesOptions{allowNoIndices: true, expandClosed: true}
	// delete index never resolves aliases
	deleteIndexOptions = indicesOptions{allowNoIndices: true, expandOpen: true, expandClosed: true, ignoreAliases: true}
	// put mapping / settings do not allow an expression matching nothing
	updateIndicesOptions = indicesOptions{expandOpen: true, expandClosed: true}
	// alias actions resolve concrete indices only
	aliasActionOptions = indicesOptions{allowNoIndices: true, expandOpen: true, expandClosed: true, ignoreAliases: true}
)

// indicesOptionParams are the request parameters that change the indices
// options of a request.
var indicesOptionParams = []string{"expand_wildcards", "ignore_unavailable", "allow_no_indices", "ignore_throttled"}

// withParams applies expand_wildcards, ignore_unavailable and
// allow_no_indices (IndicesOptions.fromRequest).
func (o indicesOptions) withParams(p Params) (indicesOptions, error) {
	if p == nil {
		return o, nil
	}
	if v, ok := p["expand_wildcards"]; ok {
		var err error
		if o.expandOpen, o.expandClosed, o.expandHidden, err = parseExpandWildcards(v); err != nil {
			return o, err
		}
	}
	var err error
	if o.ignoreUnavailable, err = optionBool(p, "ignore_unavailable", o.ignoreUnavailable); err != nil {
		return o, err
	}
	if o.allowNoIndices, err = optionBool(p, "allow_no_indices", o.allowNoIndices); err != nil {
		return o, err
	}
	if _, err = optionBool(p, "ignore_throttled", true); err != nil {
		return o, err
	}
	return o, nil
}

// withValues applies the indices options of an msearch header.
func (o indicesOptions) withValues(header M) (indicesOptions, error) {
	p := Params{}
	for _, pair := range [][2]string{{"expand_wildcards", "expandWildcards"}, {"ignore_unavailable", "ignoreUnavailable"}, {"allow_no_indices", "allowNoIndices"}, {"ignore_throttled", "ignoreThrottled"}} {
		for _, key := range pair {
			if v, ok := header[key]; ok {
				p[pair[0]] = headerString(v)
			}
		}
	}
	return o.withParams(p)
}

func headerString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, headerString(e))
		}
		return strings.Join(parts, ",")
	}
	return getString(M{"v": v}, "v")
}

// parseExpandWildcards is IndicesOptions.WildcardStates.parseParameter.
func parseExpandWildcards(v string) (open, closed, hidden bool, err error) {
	for _, w := range splitCommaJava(v) {
		switch w {
		case "open":
			open = true
		case "closed":
			closed = true
		case "hidden":
			hidden = true
		case "none":
			open, closed, hidden = false, false, false
		case "all":
			open, closed, hidden = true, true, true
		default:
			return false, false, false, errIllegalArgument("No valid expand wildcard value [%s]", w)
		}
	}
	return open, closed, hidden, nil
}

// optionBool parses a boolean indices option the way XContentMapValues
// nodeBooleanValue does: blank values keep the default.
func optionBool(p Params, name string, def bool) (bool, error) {
	v, ok := p[name]
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return def, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Could not convert [" + name + "] to boolean",
		Cause: &Error{Type: "illegal_argument_exception", Reason: "Failed to parse value [" + v + "] as only [true] or [false] are allowed."}}
}

// splitCommaJava splits like String.split(","): inner empty parts are kept,
// trailing empty parts are dropped, nothing is trimmed.
func splitCommaJava(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func indexHidden(ix *Index) bool {
	return getBool(getMap(ix.Settings, "index"), "hidden", false)
}

// indexAbstraction is an index or an alias.
type indexAbstraction struct {
	name    string
	alias   bool
	hidden  bool
	indices []*Index
}

// exprResolver resolves expressions against one snapshot of the indices.
type exprResolver struct {
	c      *Cluster
	lookup map[string]*indexAbstraction
	names  []string // sorted abstraction names
	all    []string // sorted index names
}

func (c *Cluster) newExprResolver() *exprResolver {
	r := &exprResolver{c: c, lookup: map[string]*indexAbstraction{}, all: c.sortedIndexNames()}
	for _, name := range r.all {
		ix := c.indices[name]
		r.lookup[name] = &indexAbstraction{name: name, hidden: indexHidden(ix), indices: []*Index{ix}}
	}
	for _, name := range r.all {
		ix := c.indices[name]
		for aliasName, a := range ix.Aliases {
			ab := r.lookup[aliasName]
			if ab == nil {
				ab = &indexAbstraction{name: aliasName, alias: true}
				r.lookup[aliasName] = ab
			}
			if !ab.alias {
				continue
			}
			ab.indices = append(ab.indices, ix)
			if a.IsHidden != nil && *a.IsHidden {
				ab.hidden = true
			}
		}
	}
	for name := range r.lookup {
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)
	return r
}

// starMatch is Regex.simpleMatch: only '*' is a wildcard.
func starMatch(pattern, s string) bool {
	i := strings.IndexByte(pattern, '*')
	if i < 0 {
		return pattern == s
	}
	if !strings.HasPrefix(s, pattern[:i]) {
		return false
	}
	rest, tail := s[i:], pattern[i+1:]
	for strings.HasPrefix(tail, "*") {
		tail = tail[1:]
	}
	if tail == "" {
		return true
	}
	for j := 0; j <= len(rest); j++ {
		if starMatch(tail, rest[j:]) {
			return true
		}
	}
	return false
}

type orderedNames struct {
	list []string
	set  map[string]bool
}

func newOrderedNames(names []string) *orderedNames {
	o := &orderedNames{set: map[string]bool{}}
	for _, n := range names {
		o.add(n)
	}
	return o
}

func (o *orderedNames) add(n string) {
	if !o.set[n] {
		o.set[n] = true
		o.list = append(o.list, n)
	}
}

func (o *orderedNames) remove(n string) {
	if !o.set[n] {
		return
	}
	delete(o.set, n)
	for i, e := range o.list {
		if e == n {
			o.list = append(o.list[:i], o.list[i+1:]...)
			break
		}
	}
}

// expand is WildcardExpressionResolver.resolve. preserveAliases keeps the
// names of aliases matched by a wildcard instead of their indices.
func (r *exprResolver) expand(exprs []string, o indicesOptions, preserveAliases bool) ([]string, error) {
	if !o.expandOpen && !o.expandClosed {
		return exprs, nil
	}
	if len(exprs) == 0 || (len(exprs) == 1 && (exprs[0] == "_all" || exprs[0] == "*")) {
		return r.allIndices(o), nil
	}
	var result *orderedNames
	wildcardSeen := false
	for i, expr := range exprs {
		if expr == "" {
			e := errIndexNotFound(expr)
			e.Extra["index"] = ""
			e.Extra["index_uuid"] = "_na_"
			return nil, e
		}
		if expr[0] == '_' {
			return nil, &Error{Status: http.StatusBadRequest, Type: "invalid_index_name_exception", Reason: "Invalid index name [" + expr + "], must not start with '_'.", Index: expr}
		}
		if ab := r.lookup[expr]; ab != nil && !(ab.alias && o.ignoreAliases) {
			if result != nil {
				result.add(expr)
			}
			continue
		}
		add := true
		if expr[0] == '-' && wildcardSeen {
			add = false
			expr = expr[1:]
		}
		if result == nil {
			result = newOrderedNames(exprs[:i])
		}
		if !strings.Contains(expr, "*") {
			if !o.ignoreUnavailable {
				ab := r.lookup[expr]
				if ab == nil {
					return nil, errIndexNotFound(expr)
				}
				if ab.alias && o.ignoreAliases {
					return nil, errAliasesNotSupported(expr)
				}
			}
			if add {
				result.add(expr)
			} else {
				result.remove(expr)
			}
			continue
		}
		var matches []*indexAbstraction
		for _, name := range r.names {
			ab := r.lookup[name]
			if ab.alias && o.ignoreAliases {
				continue
			}
			if starMatch(expr, name) {
				matches = append(matches, ab)
			}
		}
		for _, name := range r.expandMatches(matches, expr, o, preserveAliases) {
			if add {
				result.add(name)
			} else {
				result.remove(name)
			}
		}
		if !o.allowNoIndices && len(matches) == 0 {
			return nil, errIndexNotFound(expr)
		}
		wildcardSeen = true
	}
	if result == nil {
		return exprs, nil
	}
	if len(result.list) == 0 && !o.allowNoIndices {
		return nil, errIndexNotFoundNull("index_or_alias", exprs, "")
	}
	return result.list, nil
}

func (r *exprResolver) expandMatches(matches []*indexAbstraction, expr string, o indicesOptions, preserveAliases bool) []string {
	var out []string
	for _, ab := range matches {
		implicitHidden := strings.HasPrefix(ab.name, ".") && strings.HasPrefix(expr, ".")
		if ab.hidden && !o.expandHidden && !implicitHidden {
			continue
		}
		if preserveAliases && ab.alias {
			out = append(out, ab.name)
			continue
		}
		for _, ix := range ab.indices {
			if (ix.stateClosed && o.expandClosed) || (!ix.stateClosed && o.expandOpen) {
				out = append(out, ix.Name)
			}
		}
	}
	return out
}

// allIndices resolves "", "_all" and "*".
func (r *exprResolver) allIndices(o indicesOptions) []string {
	var out []string
	for _, name := range r.all {
		ix := r.c.indices[name]
		if indexHidden(ix) && !o.expandHidden {
			continue
		}
		if (ix.stateClosed && !o.expandClosed) || (!ix.stateClosed && !o.expandOpen) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// concrete is IndexNameExpressionResolver.concreteIndices.
func (r *exprResolver) concrete(original []string, o indicesOptions) ([]*Index, error) {
	exprs := original
	if len(exprs) == 0 {
		exprs = []string{"_all"}
	}
	failNoIndices := !o.ignoreUnavailable
	if len(exprs) == 1 {
		failNoIndices = !o.allowNoIndices
	}
	resolved, err := r.expand(exprs, o, false)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		if !o.allowNoIndices {
			if len(exprs) == 1 && exprs[0] == "_all" {
				return nil, errIndexNotFoundNull("index_expression", exprs, " and no indices exist")
			}
			return nil, errIndexNotFoundNull("index_expression", exprs, "")
		}
		return nil, nil
	}
	failClosed := o.forbidClosed && !o.ignoreUnavailable
	seen := map[*Index]bool{}
	var out []*Index
	for _, name := range resolved {
		ab := r.lookup[name]
		if ab == nil {
			if failNoIndices {
				e := errIndexNotFound(name)
				if name == "_all" {
					e.Reason = "no such index [_all] and no indices exist"
				}
				e.Extra["resource.type"] = "index_expression"
				return nil, e
			}
			continue
		}
		if ab.alias && o.ignoreAliases {
			if failNoIndices {
				return nil, errAliasesNotSupported(name)
			}
			continue
		}
		for _, ix := range ab.indices {
			if ix.stateClosed {
				if failClosed {
					if o.expandClosed && !o.expandOpen && !o.expandHidden {
						return nil, errIllegalArgument("To expand [CLOSE] wildcard, please set forbid_closed_indices to `false`")
					}
					return nil, errIndexClosed(ix)
				}
				if o.forbidClosed {
					continue
				}
			}
			if !seen[ix] {
				seen[ix] = true
				out = append(out, ix)
			}
		}
	}
	if !o.allowNoIndices && len(out) == 0 {
		return nil, errIndexNotFoundNull("index_expression", exprs, "")
	}
	return out, nil
}

// filteringAliases is IndexNameExpressionResolver.indexAliases for the alias
// filters of one index: nil when the index is reached without a filter.
func filteringAliases(ix *Index, resolved []string) []string {
	if len(resolved) == 0 || (len(resolved) == 1 && resolved[0] == "_all") {
		return nil
	}
	if len(resolved) == 1 {
		if a := ix.Aliases[resolved[0]]; a != nil && a.Filter != nil {
			return []string{resolved[0]}
		}
		return nil
	}
	var out []string
	for _, name := range resolved {
		if name == ix.Name {
			return nil
		}
		if a := ix.Aliases[name]; a != nil {
			if a.Filter == nil {
				return nil
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// aliasFilter combines the filters of several aliases with a should clause.
func aliasFilter(ix *Index, aliases []string) M {
	switch len(aliases) {
	case 0:
		return nil
	case 1:
		return ix.Aliases[aliases[0]].Filter
	}
	should := make([]any, 0, len(aliases))
	for _, name := range aliases {
		should = append(should, ix.Aliases[name].Filter)
	}
	return M{"bool": M{"should": should}}
}

// targets resolves an expression to targets; search-like requests
// (withFilters) carry the alias filters of each index.
func (r *exprResolver) targets(expr string, o indicesOptions, withFilters bool) ([]target, error) {
	exprs := splitCommaJava(expr)
	indices, err := r.concrete(exprs, o)
	if err != nil {
		return nil, err
	}
	ts := make([]target, len(indices))
	var resolved []string
	if withFilters && len(indices) > 0 {
		resolved, _ = r.expand(exprs, lenientExpandOpenOptions, true)
	}
	for i, ix := range indices {
		ts[i] = target{ix: ix}
		if withFilters {
			ts[i].filter = aliasFilter(ix, filteringAliases(ix, resolved))
		}
	}
	return ts, nil
}

// resolveIndices resolves an expression with default options updated from
// the request parameters.
func (c *Cluster) resolveIndices(expr string, p Params, defaults indicesOptions) ([]*Index, error) {
	o, err := defaults.withParams(p)
	if err != nil {
		return nil, err
	}
	return c.newExprResolver().concrete(splitCommaJava(expr), o)
}

// errIndexNotFoundNull is IndexNotFoundException((String) null) carrying the
// expressions as resources.
func errIndexNotFoundNull(resourceType string, ids []string, suffix string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "index_not_found_exception", Reason: "no such index [null]" + suffix,
		Extra: map[string]any{"resource.type": resourceType, "resource.id": resourceIDs(ids)}}
}

// resourceIDs renders exception metadata: one value as a string, several as
// an array.
func resourceIDs(ids []string) any {
	if len(ids) == 1 {
		return ids[0]
	}
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func errAliasesNotSupported(expr string) *Error {
	return errIllegalArgument("The provided expression [%s] matches an alias, specify the corresponding concrete indices instead.", expr)
}

// errIndexClosed is IndexClosedException.
func errIndexClosed(ix *Index) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "index_closed_exception", Reason: "closed", Index: ix.Name,
		Extra: map[string]any{"index_uuid": ix.UUID}}
}

// open and close ---------------------------------------------------------

// dropPITContexts frees the point in time reader contexts of an index that
// is closed or deleted: searches of those points in time fail on its shards.
func (c *Cluster) dropPITContexts(name string) {
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	for _, pit := range c.pits {
		for _, t := range pit.targets {
			if t.ix.Name == name {
				if pit.lost == nil {
					pit.lost = map[string]bool{}
				}
				pit.lost[name] = true
			}
		}
	}
}

// parseActiveShardCount validates wait_for_active_shards
// (ActiveShardCount.parseString).
func parseActiveShardCount(p Params) error {
	v, ok := p["wait_for_active_shards"]
	if !ok {
		return nil
	}
	if v == "all" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "cannot parse ActiveShardCount[" + v + "]",
			Cause: &Error{Type: "number_format_exception", Reason: "For input string: \"" + v + "\""}}
	}
	if n < 0 {
		return errIllegalArgument("shard count cannot be a negative value")
	}
	return nil
}

// checkAckTimeouts parses the timeouts of acknowledged index requests.
func checkAckTimeouts(p Params) error {
	for _, k := range []string{"timeout", "cluster_manager_timeout", "master_timeout"} {
		if v, ok := p[k]; ok {
			if _, err := parseTimeValue(v, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// CloseIndex implements POST /{index}/_close.
func (c *Cluster) CloseIndex(expr string, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkAckTimeouts(p); err != nil {
		return fail(err)
	}
	if err := parseActiveShardCount(p); err != nil {
		return fail(err)
	}
	indices, err := c.resolveIndices(expr, p, strictExpandOpenOptions)
	if err != nil {
		return fail(err)
	}
	closed := M{}
	for _, t := range indices {
		if t.stateClosed {
			continue
		}
		ix, err := c.writable(t.Name)
		if err != nil {
			return fail(err)
		}
		ix.stateClosed = true
		setNested(ix.Settings, "index.verified_before_close", "true")
		closed[ix.Name] = M{"closed": true}
		c.dropPITContexts(ix.Name)
	}
	return ok(M{"acknowledged": true, "shards_acknowledged": len(closed) > 0, "indices": closed})
}

// OpenIndex implements POST /{index}/_open.
func (c *Cluster) OpenIndex(expr string, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkAckTimeouts(p); err != nil {
		return fail(err)
	}
	if err := parseActiveShardCount(p); err != nil {
		return fail(err)
	}
	indices, err := c.resolveIndices(expr, p, openIndexOptions)
	if err != nil {
		return fail(err)
	}
	for _, t := range indices {
		if !t.stateClosed {
			continue
		}
		ix, err := c.writable(t.Name)
		if err != nil {
			return fail(err)
		}
		if ix.reopenRebuild {
			// settings that shape the analysis changed while the index was
			// closed: rebuild the analyzers and re-index
			n, err := ix.copyIndex()
			if err != nil {
				return fail(err)
			}
			ix.release()
			c.indices[n.Name] = n
			ix = n
			ix.reopenRebuild = false
		}
		ix.stateClosed = false
		if idx := getMap(ix.Settings, "index"); idx != nil {
			delete(idx, "verified_before_close")
		}
	}
	return ok(M{"acknowledged": true, "shards_acknowledged": true})
}

// index shard counts ------------------------------------------------------

func indexShardCount(ix *Index) int {
	n := getInt(getMap(ix.Settings, "index"), "number_of_shards", 1)
	if n < 1 {
		n = 1
	}
	return n
}

func indexReplicaCount(ix *Index) int {
	n := getInt(getMap(ix.Settings, "index"), "number_of_replicas", 1)
	if n < 0 {
		n = 0
	}
	return n
}

// broadcastShards is the _shards section of broadcast responses (refresh,
// flush, stats): every shard copy counts, only primaries succeed on a
// single node.
func broadcastShards(indices []*Index) M {
	total, successful := 0, 0
	for _, ix := range indices {
		shards := indexShardCount(ix)
		total += shards * (1 + indexReplicaCount(ix))
		successful += shards
	}
	return M{"total": total, "successful": successful, "failed": 0}
}
