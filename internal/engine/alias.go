package engine

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// aliasDef is a parsed alias definition: an add action of the update aliases
// API, an alias of a create index request or an alias of a template.
type aliasDef struct {
	name          string
	filter        M
	routing       *string
	indexRouting  *string
	searchRouting *string
	isWriteIndex  *bool
	isHidden      *bool
}

// The routing parameter sets both routings unless they are given explicitly.
func (d *aliasDef) effectiveIndexRouting() *string {
	if d.indexRouting != nil {
		return d.indexRouting
	}
	return d.routing
}

func (d *aliasDef) effectiveSearchRouting() *string {
	if d.searchRouting != nil {
		return d.searchRouting
	}
	return d.routing
}

// toAlias builds the stored alias metadata (routing is always stored as
// index and search routing, the way AliasMetadata keeps it).
func (d *aliasDef) toAlias() *Alias {
	a := &Alias{}
	if len(d.filter) > 0 {
		a.Filter = cloneDeep(d.filter).(M)
	}
	if r := d.effectiveIndexRouting(); r != nil {
		a.IndexRouting = *r
	}
	if r := d.effectiveSearchRouting(); r != nil {
		a.SearchRouting = *r
	}
	if d.isWriteIndex != nil {
		b := *d.isWriteIndex
		a.IsWriteIndex = &b
	}
	if d.isHidden != nil {
		b := *d.isHidden
		a.IsHidden = &b
	}
	return a
}

// aliasDefJSON renders a definition the way AliasMetadata is rendered.
func aliasDefJSON(d *aliasDef) M {
	out := M{}
	if len(d.filter) > 0 {
		out["filter"] = cloneDeep(d.filter)
	}
	if r := d.effectiveIndexRouting(); r != nil {
		out["index_routing"] = *r
	}
	if r := d.effectiveSearchRouting(); r != nil {
		out["search_routing"] = *r
	}
	if d.isWriteIndex != nil {
		out["is_write_index"] = *d.isWriteIndex
	}
	if d.isHidden != nil {
		out["is_hidden"] = *d.isHidden
	}
	return out
}

// aliasJSON renders an alias of an index (AliasMetadata.toXContent).
func (c *Cluster) aliasJSON(ix *Index, name string, a *Alias) M {
	return a.toJSON()
}

// errors ------------------------------------------------------------------

func xContentErr(reason string, cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: reason, Cause: cause}
}

func javaIllegalArgument(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: reason}
}

// xContentToken names the JSON token of a decoded value.
func xContentToken(v any) string {
	switch v.(type) {
	case nil:
		return "VALUE_NULL"
	case string:
		return "VALUE_STRING"
	case bool:
		return "VALUE_BOOLEAN"
	case json.Number, float64, float32, int, int64:
		return "VALUE_NUMBER"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

func errAliasesNotFound(names []string) *Error {
	var id any = names[0]
	if len(names) > 1 {
		ids := make([]any, len(names))
		for i, n := range names {
			ids[i] = n
		}
		id = ids
	}
	return &Error{Status: http.StatusNotFound, Type: "aliases_not_found_exception", Reason: "aliases [" + strings.Join(names, ", ") + "] missing",
		Extra: M{"resource.type": "aliases", "resource.id": id}}
}

func errInvalidAliasName(name, description string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_alias_name_exception", Reason: "Invalid alias name [" + name + "]: " + description}
}

// validateAliasName is AliasValidator.validateAliasStandalone.
func validateAliasName(name string, indexRouting *string) error {
	if strings.TrimSpace(name) == "" {
		return javaIllegalArgument("alias name is required")
	}
	if strings.ContainsAny(name, "\\/*?\"<>| ,") {
		return errInvalidAliasName(name, `must not contain the following characters [ , ", *, \, <, |, ,, >, /, ?]`)
	}
	if strings.Contains(name, "#") {
		return errInvalidAliasName(name, "must not contain '#'")
	}
	if strings.Contains(name, ":") {
		return errInvalidAliasName(name, "must not contain ':'")
	}
	if name[0] == '_' || name[0] == '-' || name[0] == '+' {
		return errInvalidAliasName(name, "must not start with '_', '-', or '+'")
	}
	if len(name) > 255 {
		return errInvalidAliasName(name, "index name is too long, ("+strconv.Itoa(len(name))+" > 255)")
	}
	if name == "." || name == ".." {
		return errInvalidAliasName(name, "must not be '.' or '..'")
	}
	if indexRouting != nil && strings.Contains(*indexRouting, ",") {
		return javaIllegalArgument("alias [" + name + "] has several index routing values associated with it")
	}
	return nil
}

// validateAliasFilter parses an alias filter against the target index.
func (c *Cluster) validateAliasFilter(ix *Index, alias string, filter M) error {
	if len(filter) == 0 || ix == nil {
		return nil
	}
	qb := &queryBuilder{c: c, ix: ix}
	if _, err := qb.build(filter); err != nil {
		cause, ok := err.(*Error)
		if !ok {
			cause = &Error{Type: "exception", Reason: err.Error()}
		}
		// the filter is parsed from its compact JSON rendering
		newCompactFrame(filter, nil).mark(cause)
		return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "failed to parse filter for alias [" + alias + "]", Cause: cause}
	}
	return nil
}

func parseBooleanValue(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, javaIllegalArgument("Failed to parse value [" + t + "] as only [true] or [false] are allowed.")
	}
	return false, javaIllegalArgument("Failed to parse value [" + valueText(v) + "] as only [true] or [false] are allowed.")
}

func valueText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// update aliases -----------------------------------------------------------

// aliasAction is one action of an update aliases request.
type aliasAction struct {
	kind      string // add, remove or remove_index
	indices   []string
	aliases   []string
	def       aliasDef
	mustExist *bool
	// fromPath marks actions built from URL parameters (index expressions
	// are comma separated there).
	fromPath bool
}

// parseAliasActions is IndicesAliasesRequest.PARSER.
func parseAliasActions(body M) ([]*aliasAction, error) {
	for _, k := range sortedMapKeys(body) {
		if k != "actions" {
			return nil, xContentErr("[aliases] unknown field ["+k+"]", nil).at(keyTok(body, k)).atParser(valueTok(body, k))
		}
	}
	raw, present := body["actions"]
	if !present {
		return nil, nil
	}
	var list []any
	switch t := raw.(type) {
	case []any:
		list = t
	case M:
		list = []any{t}
	default:
		return nil, xContentErr("[aliases] actions doesn't support values of type: "+xContentToken(raw), nil).at(valueTok(body, "actions"))
	}
	wrap := func(inner *Error) *Error {
		return xContentErr("[aliases] failed to parse field [actions]", inner).atCause(valueEndTok(body, "actions"))
	}
	actions := make([]*aliasAction, 0, len(list))
	for _, item := range list {
		m, ok := item.(M)
		if !ok {
			return nil, wrap(xContentErr("[alias_action] Expected START_OBJECT but was: "+xContentToken(item), nil).at(noTok))
		}
		var action *aliasAction
		count := 0
		for _, kind := range sortedMapKeys(m) {
			switch kind {
			case "add", "remove", "remove_index":
			default:
				return nil, wrap(xContentErr("[alias_action] unknown field ["+kind+"]", nil).at(keyTok(m, kind)).atParser(valueTok(m, kind)))
			}
			spec, ok := m[kind].(M)
			if !ok {
				return nil, wrap(xContentErr("[alias_action] "+kind+" doesn't support values of type: "+xContentToken(m[kind]), nil).at(valueTok(m, kind)))
			}
			a, err := parseAliasActionSpec(kind, spec)
			if err != nil {
				return nil, wrap(xContentErr("[alias_action] failed to parse field ["+kind+"]", err).atCause(valueEndTok(m, kind)))
			}
			action = a
			count++
		}
		if count > 1 {
			return nil, wrap(xContentErr("Failed to build [alias_action] after last required field arrived", javaIllegalArgument("Too many operations declared on operation entry")).
				atParser(endTok(m)))
		}
		actions = append(actions, action)
	}
	for _, a := range actions {
		if a == nil {
			return nil, wrap(&Error{Type: "null_pointer_exception", Reason: `Cannot invoke "org.opensearch.action.admin.indices.alias.IndicesAliasesRequest$AliasActions.validate()" because "aliasAction" is null`})
		}
		if a.indices == nil {
			return nil, wrap(javaIllegalArgument("One of [index] or [indices] is required"))
		}
		if a.kind != "remove_index" && len(a.aliases) == 0 {
			return nil, wrap(javaIllegalArgument("One of [alias] or [aliases] is required"))
		}
	}
	return actions, nil
}

func stringListStrict(v any) ([]string, bool) {
	switch t := v.(type) {
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// parseAliasActionSpec parses the body of one add/remove/remove_index action.
func parseAliasActionSpec(kind string, spec M) (*aliasAction, *Error) {
	a := &aliasAction{kind: kind}
	upper := strings.ToUpper(kind)
	fieldErr := func(field string, cause *Error) *Error {
		return xContentErr("["+kind+"] failed to parse field ["+field+"]", cause).atCause(valueEndTok(spec, field))
	}
	typeErr := func(field string, v any) *Error {
		return xContentErr("["+kind+"] "+field+" doesn't support values of type: "+xContentToken(v), nil).at(valueTok(spec, field))
	}
	unknown := func(field string) *Error {
		return xContentErr("["+kind+"] unknown field ["+field+"]", nil).at(keyTok(spec, field)).atParser(valueTok(spec, field))
	}
	for _, field := range sortedMapKeys(spec) {
		v := spec[field]
		switch field {
		case "index":
			s, ok := v.(string)
			if !ok {
				return nil, typeErr(field, v)
			}
			if a.indices != nil {
				return nil, fieldErr(field, javaIllegalArgument("Only one of [index] and [indices] is supported"))
			}
			if s == "" {
				return nil, fieldErr(field, javaIllegalArgument("[index] can't be empty string"))
			}
			a.indices = []string{s}
		case "indices":
			list, ok := stringListStrict(v)
			if !ok {
				return nil, typeErr(field, v)
			}
			if a.indices != nil {
				return nil, fieldErr(field, javaIllegalArgument("Only one of [index] and [indices] is supported"))
			}
			if len(list) == 0 {
				return nil, fieldErr(field, javaIllegalArgument("[indices] can't be empty"))
			}
			for _, s := range list {
				if s == "" {
					return nil, fieldErr(field, javaIllegalArgument("[indices] can't contain empty string"))
				}
			}
			a.indices = list
		case "alias":
			s, ok := v.(string)
			if !ok {
				return nil, typeErr(field, v)
			}
			if len(a.aliases) > 0 {
				return nil, fieldErr(field, javaIllegalArgument("Only one of [alias] and [aliases] is supported"))
			}
			if kind == "remove_index" {
				return nil, fieldErr(field, javaIllegalArgument("[alias] is unsupported for ["+upper+"]"))
			}
			if s == "" {
				return nil, fieldErr(field, javaIllegalArgument("[alias] can't be empty string"))
			}
			a.aliases = []string{s}
		case "aliases":
			list, ok := stringListStrict(v)
			if !ok {
				return nil, typeErr(field, v)
			}
			if len(a.aliases) > 0 {
				return nil, fieldErr(field, javaIllegalArgument("Only one of [alias] and [aliases] is supported"))
			}
			if kind == "remove_index" {
				return nil, fieldErr(field, javaIllegalArgument("[aliases] is unsupported for ["+upper+"]"))
			}
			if len(list) == 0 {
				return nil, fieldErr(field, javaIllegalArgument("[aliases] can't be empty"))
			}
			for _, s := range list {
				if s == "" {
					return nil, fieldErr(field, javaIllegalArgument("[aliases] can't contain empty string"))
				}
			}
			a.aliases = list
		case "filter":
			if kind != "add" {
				return nil, unknown(field)
			}
			m, ok := v.(M)
			if !ok {
				return nil, typeErr(field, v)
			}
			a.def.filter = m
		case "routing", "index_routing", "search_routing":
			if kind != "add" {
				return nil, unknown(field)
			}
			switch v.(type) {
			case string, json.Number, float64:
			default:
				return nil, typeErr(field, v)
			}
			s := valueText(v)
			switch field {
			case "routing":
				a.def.routing = &s
			case "index_routing":
				a.def.indexRouting = &s
			default:
				a.def.searchRouting = &s
			}
		case "is_write_index", "is_hidden":
			if kind != "add" {
				return nil, unknown(field)
			}
			switch v.(type) {
			case bool, string:
			default:
				return nil, typeErr(field, v)
			}
			b, err := parseBooleanValue(v)
			if err != nil {
				return nil, fieldErr(field, err.(*Error))
			}
			if field == "is_write_index" {
				a.def.isWriteIndex = &b
			} else {
				a.def.isHidden = &b
			}
		case "must_exist":
			if kind != "remove" {
				return nil, unknown(field)
			}
			switch v.(type) {
			case bool, string:
			default:
				return nil, typeErr(field, v)
			}
			b, err := parseBooleanValue(v)
			if err != nil {
				return nil, fieldErr(field, err.(*Error))
			}
			a.mustExist = &b
		default:
			return nil, unknown(field)
		}
	}
	return a, nil
}

// UpdateAliases implements POST /_aliases.
func (c *Cluster) UpdateAliases(body M) (Response, error) {
	actions, err := parseAliasActions(body)
	if err != nil {
		return fail(err)
	}
	if len(actions) == 0 {
		return fail(javaIllegalArgument("No action specified"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.applyAliasActions(actions); err != nil {
		return fail(err)
	}
	return ok(M{"acknowledged": true})
}

// aliasActionIndices resolves the index expressions of an alias action to
// concrete index names. Aliases are not accepted as targets and wildcards
// must match at least one index.
func (c *Cluster) aliasActionIndices(a *aliasAction) ([]string, error) {
	var exprs []string
	for _, e := range a.indices {
		if a.fromPath {
			exprs = append(exprs, splitList(e)...)
		} else {
			exprs = append(exprs, e)
		}
	}
	if len(exprs) == 0 {
		return nil, javaIllegalArgument("[indices] can't be empty")
	}
	seen := map[string]bool{}
	var out []string
	for _, expr := range exprs {
		if !a.fromPath && strings.Contains(expr, ",") {
			return nil, errIndexNotFound(expr)
		}
		if _, isIndex := c.indices[expr]; !isIndex && !strings.ContainsAny(expr, "*?") && expr != "_all" && len(c.aliasTargets(expr)) > 0 {
			return nil, javaIllegalArgument("The provided expression [" + expr + "] matches an alias, specify the corresponding concrete indices instead.")
		}
		ts, err := c.resolve(expr, resolveOptions{allowAliases: false, allowNoIndices: false})
		if err != nil {
			return nil, err
		}
		if len(ts) == 0 {
			return nil, errIndexNotFound(expr)
		}
		for _, t := range ts {
			if !seen[t.ix.Name] {
				seen[t.ix.Name] = true
				out = append(out, t.ix.Name)
			}
		}
	}
	return out, nil
}

// aliasPatternMatches reports whether an alias name matches the alias
// patterns of a request (wildcards, _all, and "-" exclusions after the first
// wildcard), the way Metadata.findAliases does.
func aliasPatternMatches(patterns []string, name string) bool {
	firstWildcard := len(patterns)
	for i, p := range patterns {
		if strings.ContainsAny(p, "*?") {
			firstWildcard = i
			break
		}
	}
	matched := false
	for i, p := range patterns {
		if i > firstWildcard && strings.HasPrefix(p, "-") {
			if wildcardMatch(p[1:], name) {
				matched = false
			}
			continue
		}
		if p == "_all" || p == "*" || wildcardMatch(p, name) {
			matched = true
		}
	}
	return matched
}

type finalAliasAction struct {
	kind      string
	index     string
	alias     string
	def       *aliasDef
	mustExist *bool
}

type aliasEntry struct {
	alias  *Alias
	hidden *bool
}

// applyAliasActions is TransportIndicesAliasesAction followed by
// MetadataIndexAliasesService: remove actions are expanded against the
// aliases that exist before the request, index removals run first, the
// remaining actions run in order and the result is validated before it is
// committed atomically.
func (c *Cluster) applyAliasActions(actions []*aliasAction) error {
	// TransportIndicesAliasesAction.checkBlock: metadata write blocks of the
	// indices named in the request
	var named []*Index
	for _, a := range actions {
		for _, e := range a.indices {
			names := []string{e}
			if a.fromPath {
				names = splitList(e)
			}
			for _, n := range names {
				if ix, ok := c.indices[n]; ok {
					named = append(named, ix)
				}
			}
		}
	}
	if err := c.checkBlocks(named, blockMetadataWrite); err != nil {
		return err
	}
	var final []finalAliasAction
	var requested []string
	seenRequested := map[string]bool{}
	for _, a := range actions {
		names, err := c.aliasActionIndices(a)
		if err != nil {
			return err
		}
		for _, al := range a.aliases {
			if !seenRequested[al] {
				seenRequested[al] = true
				requested = append(requested, al)
			}
		}
		for _, name := range names {
			ix := c.indices[name]
			switch a.kind {
			case "add":
				for _, al := range a.aliases {
					final = append(final, finalAliasAction{kind: "add", index: name, alias: al, def: &a.def})
				}
			case "remove":
				if a.mustExist != nil && !*a.mustExist {
					for _, al := range a.aliases {
						final = append(final, finalAliasAction{kind: "remove", index: name, alias: al, mustExist: a.mustExist})
					}
					continue
				}
				var existing []string
				for an := range ix.Aliases {
					if aliasPatternMatches(a.aliases, an) {
						existing = append(existing, an)
					}
				}
				sort.Strings(existing)
				if a.mustExist != nil {
					var missing []string
					for _, p := range a.aliases {
						found := false
						for an := range ix.Aliases {
							if p == "_all" || wildcardMatch(p, an) {
								found = true
								break
							}
						}
						if !found {
							missing = append(missing, p)
						}
					}
					if len(missing) > 0 {
						return errAliasesNotFound(missing)
					}
				}
				for _, an := range existing {
					final = append(final, finalAliasAction{kind: "remove", index: name, alias: an, mustExist: a.mustExist})
				}
			case "remove_index":
				final = append(final, finalAliasAction{kind: "remove_index", index: name})
			}
		}
	}
	if len(final) == 0 {
		return errAliasesNotFound(requested)
	}

	removed := map[string]bool{}
	for _, fa := range final {
		if fa.kind == "remove_index" {
			removed[fa.index] = true
		}
	}
	work := map[string]map[string]aliasEntry{}
	entries := func(name string) map[string]aliasEntry {
		if m, ok := work[name]; ok {
			return m
		}
		ix := c.indices[name]
		m := make(map[string]aliasEntry, len(ix.Aliases))
		for an, al := range ix.Aliases {
			m[an] = aliasEntry{alias: al, hidden: al.IsHidden}
		}
		work[name] = m
		return m
	}
	touched := map[string]bool{}
	for _, fa := range final {
		if fa.kind == "remove_index" {
			continue
		}
		ix, exists := c.indices[fa.index]
		if !exists || removed[fa.index] {
			return errIndexNotFound(fa.index)
		}
		switch fa.kind {
		case "add":
			if err := validateAliasName(fa.alias, fa.def.effectiveIndexRouting()); err != nil {
				return err
			}
			if other, isIndex := c.indices[fa.alias]; isIndex && !removed[fa.alias] {
				return &Error{Status: http.StatusBadRequest, Type: "invalid_alias_name_exception", Reason: "Invalid alias name [" + fa.alias + "], an index exists with the same name as the alias", Index: other.Name}
			}
			if err := c.validateAliasFilter(ix, fa.alias, fa.def.filter); err != nil {
				return err
			}
			entries(fa.index)[fa.alias] = aliasEntry{alias: fa.def.toAlias(), hidden: fa.def.isHidden}
			touched[fa.alias] = true
		case "remove":
			m := entries(fa.index)
			if _, ok := m[fa.alias]; !ok {
				if fa.mustExist != nil && *fa.mustExist {
					return &Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "required alias [" + fa.alias + "] does not exist"}
				}
				continue
			}
			delete(m, fa.alias)
		}
	}
	// validate the alias properties of the resulting metadata
	for alias := range touched {
		if err := c.validateAliasProperties(alias, removed, work); err != nil {
			return err
		}
	}
	// commit
	for name := range removed {
		if ix, ok := c.indices[name]; ok {
			ix.release()
			delete(c.indices, name)
		}
	}
	names := make([]string, 0, len(work))
	for name := range work {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if removed[name] {
			continue
		}
		ix, err := c.writable(name)
		if err != nil {
			return err
		}
		aliases := make(map[string]*Alias, len(work[name]))
		for an, e := range work[name] {
			aliases[an] = e.alias
		}
		ix.Aliases = aliases
	}
	return nil
}

// validateAliasProperties checks that an alias has at most one write index
// and the same is_hidden setting on all of its indices
// (IndexAbstraction.Alias.validateAliasProperties).
func (c *Cluster) validateAliasProperties(alias string, removed map[string]bool, work map[string]map[string]aliasEntry) error {
	var withAlias []string
	props := map[string]aliasEntry{}
	for _, name := range c.sortedIndexNames() {
		if removed[name] {
			continue
		}
		var e aliasEntry
		var ok bool
		if m, changed := work[name]; changed {
			e, ok = m[alias]
		} else if a, has := c.indices[name].Aliases[alias]; has {
			e, ok = aliasEntry{alias: a, hidden: a.IsHidden}, true
		}
		if ok {
			withAlias = append(withAlias, name)
			props[name] = e
		}
	}
	var writeIndices, hiddenOn, visibleOn []string
	for _, name := range withAlias {
		e := props[name]
		if e.alias.IsWriteIndex != nil && *e.alias.IsWriteIndex {
			writeIndices = append(writeIndices, name)
		}
		if e.hidden != nil && *e.hidden {
			hiddenOn = append(hiddenOn, name)
		} else {
			visibleOn = append(visibleOn, name)
		}
	}
	if len(writeIndices) > 1 {
		return &Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: "alias [" + alias + "] has more than one write index [" + strings.Join(writeIndices, ",") + "]"}
	}
	if len(hiddenOn) > 0 && len(visibleOn) > 0 {
		return &Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: "alias [" + alias + "] has is_hidden set to true on indices [" + strings.Join(hiddenOn, ",") + "] but does not have is_hidden set to true on indices [" + strings.Join(visibleOn, ",") + "]; alias must have the same is_hidden setting on all indices"}
	}
	return nil
}

// PutAlias implements PUT /{index}/_alias/{name} and its variants. Body
// fields override the path parameters (RestIndexPutAliasAction).
func (c *Cluster) PutAlias(expr, name string, body M) (Response, error) {
	a := &aliasAction{kind: "add", fromPath: true}
	indices := splitList(expr)
	alias := name
	for _, field := range sortedMapKeys(body) {
		v := body[field]
		_, isObject := v.(M)
		_, isArray := v.([]any)
		if field == "filter" {
			if !isObject {
				return fail(javaIllegalArgument("unknown field [filter]"))
			}
			a.def.filter = v.(M)
			continue
		}
		if isObject || isArray {
			return fail(javaIllegalArgument("unknown field [" + field + "]"))
		}
		switch field {
		case "index":
			indices = splitList(valueText(v))
		case "alias":
			alias = valueText(v)
		case "routing":
			if v != nil {
				s := valueText(v)
				a.def.routing = &s
			}
		case "index_routing", "indexRouting", "index-routing":
			if v != nil {
				s := valueText(v)
				a.def.indexRouting = &s
			}
		case "search_routing", "searchRouting", "search-routing":
			if v != nil {
				s := valueText(v)
				a.def.searchRouting = &s
			}
		case "is_write_index", "is_hidden":
			b, err := parseBooleanValue(v)
			if err != nil {
				return fail(err)
			}
			if field == "is_write_index" {
				a.def.isWriteIndex = &b
			} else {
				a.def.isHidden = &b
			}
		default:
			return fail(javaIllegalArgument("unknown field [" + field + "]"))
		}
	}
	if len(indices) == 0 {
		return fail(javaIllegalArgument("[indices] can't be empty"))
	}
	if alias == "" {
		return fail(javaIllegalArgument("[alias] can't be empty string"))
	}
	a.indices = []string{strings.Join(indices, ",")}
	a.aliases = []string{alias}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.applyAliasActions([]*aliasAction{a}); err != nil {
		return fail(err)
	}
	return ok(M{"acknowledged": true})
}

// DeleteAlias implements DELETE /{index}/_alias/{name}.
func (c *Cluster) DeleteAlias(expr, name string) (Response, error) {
	indices := splitList(expr)
	aliases := splitList(name)
	if len(indices) == 0 {
		return fail(javaIllegalArgument("[indices] can't be empty"))
	}
	if len(aliases) == 0 {
		return fail(javaIllegalArgument("[aliases] can't be empty"))
	}
	a := &aliasAction{kind: "remove", indices: []string{strings.Join(indices, ",")}, aliases: aliases, fromPath: true}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.applyAliasActions([]*aliasAction{a}); err != nil {
		return fail(err)
	}
	return ok(M{"acknowledged": true})
}

// GetAliases implements GET /_alias, GET /{index}/_alias/{name}
// (RestGetAliasesAction).
func (c *Cluster) GetAliases(expr, name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.getAliases(expr, name, p)
}

func (c *Cluster) getAliases(expr, name string, p Params) (Response, error) {
	ts, err := c.resolve(expr, resolveOptions{params: p, base: &strictExpandHiddenOptions})
	if err != nil {
		return fail(err)
	}
	patterns := splitList(name)
	explicit := len(patterns) > 0
	out := M{}
	returned := map[string]bool{}
	seen := map[string]bool{}
	for _, t := range ts {
		ix := t.ix
		if seen[ix.Name] {
			continue
		}
		seen[ix.Name] = true
		aliases := M{}
		for an, a := range ix.Aliases {
			if explicit && !aliasPatternMatches(patterns, an) {
				continue
			}
			aliases[an] = c.aliasJSON(ix, an, a)
			returned[an] = true
		}
		if !explicit || len(aliases) > 0 {
			out[ix.Name] = M{"aliases": aliases}
		}
	}
	firstWildcard := len(patterns)
	for i, pat := range patterns {
		if strings.ContainsAny(pat, "*?") {
			firstWildcard = i
			break
		}
	}
	var missing []string
	for i, pat := range patterns {
		if pat == "_all" || strings.ContainsAny(pat, "*?") || (i > firstWildcard && strings.HasPrefix(pat, "-")) {
			continue
		}
		excluded := false
		for j := max(i+1, firstWildcard); j < len(patterns); j++ {
			if strings.HasPrefix(patterns[j], "-") && (wildcardMatch(patterns[j][1:], pat) || patterns[j][1:] == "_all") {
				excluded = true
				break
			}
		}
		if !excluded && !returned[pat] {
			missing = append(missing, pat)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		missing = dedupeSorted(missing)
		if len(missing) == 1 {
			out["error"] = "alias [" + missing[0] + "] missing"
		} else {
			out["error"] = "aliases [" + strings.Join(missing, ",") + "] missing"
		}
		out["status"] = 404
		return Response{Status: http.StatusNotFound, Body: out}, nil
	}
	return ok(out)
}

func dedupeSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// AliasExists implements HEAD /_alias/{name} and HEAD /{index}/_alias[/{name}].
func (c *Cluster) AliasExists(expr, name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, err := c.getAliases(expr, name, p)
	if err != nil {
		return Response{Status: http.StatusNotFound}, nil
	}
	if r.Status == http.StatusOK && name != "" {
		if m, ok := r.Body.(M); ok && len(m) == 0 {
			return Response{Status: http.StatusNotFound}, nil
		}
	}
	return Response{Status: r.Status}, nil
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == "_all" || wildcardMatch(p, s) {
			return true
		}
	}
	return false
}

// CatAliases lists aliases for _cat/aliases.
func (c *Cluster) CatAliases(name string) []M {
	c.mu.RLock()
	defer c.mu.RUnlock()
	patterns := splitList(name)
	var rows []M
	for _, ixName := range c.sortedIndexNames() {
		ix := c.indices[ixName]
		for an, a := range ix.Aliases {
			if len(patterns) > 0 && !aliasPatternMatches(patterns, an) {
				continue
			}
			filter := "-"
			if a.Filter != nil {
				filter = "*"
			}
			indexRouting, searchRouting := "-", "-"
			if a.IndexRouting != "" {
				indexRouting = a.IndexRouting
			}
			if a.SearchRouting != "" {
				searchRouting = a.SearchRouting
			}
			wi := "-"
			if a.IsWriteIndex != nil {
				wi = strconv.FormatBool(*a.IsWriteIndex)
			}
			rows = append(rows, M{"alias": an, "index": ixName, "filter": filter, "routing.index": indexRouting, "routing.search": searchRouting, "is_write_index": wi})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["alias"] != rows[j]["alias"] {
			return rows[i]["alias"].(string) < rows[j]["alias"].(string)
		}
		return rows[i]["index"].(string) < rows[j]["index"].(string)
	})
	return rows
}

// create index and template aliases -------------------------------------

// parseAliasObject parses one alias of a create index request or template
// body (Alias.fromXContent).
func parseAliasObject(name string, raw any) (*aliasDef, error) {
	d := &aliasDef{name: name}
	spec, ok := raw.(M)
	if !ok {
		return nil, javaIllegalArgument("No alias is specified")
	}
	// OpenSearch 3 skips fields it does not know and values of the wrong type
	for _, field := range sortedMapKeys(spec) {
		switch t := spec[field].(type) {
		case M:
			if field == "filter" {
				d.filter = t
			}
		case string:
			s := t
			switch field {
			case "routing":
				d.routing = &s
			case "index_routing", "index-routing", "indexRouting":
				d.indexRouting = &s
			case "search_routing", "search-routing", "searchRouting":
				d.searchRouting = &s
			}
		case bool:
			b := t
			switch field {
			case "is_write_index":
				d.isWriteIndex = &b
			case "is_hidden":
				d.isHidden = &b
			}
		}
	}
	return d, nil
}

// parseAliasObjects parses the aliases object of a create index request or
// template, validating alias names.
func parseAliasObjects(raw any) ([]*aliasDef, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(M)
	if !ok {
		return nil, errParsing("key [aliases] must be an object")
	}
	var out []*aliasDef
	for _, name := range sortedMapKeys(m) {
		d, err := parseAliasObject(name, m[name])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// resolveIndexAliases combines the aliases of a create index request with the
// aliases of the matching templates (request aliases win, then templates in
// the given order), replacing {index} in template alias names and validating
// the result (MetadataCreateIndexService.resolveAndValidateAliases).
func (c *Cluster) resolveIndexAliases(index string, request []*aliasDef, templates [][]*aliasDef) ([]*aliasDef, error) {
	var out []*aliasDef
	names := map[string]bool{}
	for _, d := range request {
		if err := c.validateCreateAlias(d); err != nil {
			return nil, err
		}
		names[d.name] = true
		out = append(out, d)
	}
	for _, tpl := range templates {
		for _, td := range tpl {
			d := *td
			if strings.Contains(d.name, "{index}") {
				d.name = strings.ReplaceAll(d.name, "{index}", index)
			}
			if names[td.name] || names[d.name] {
				continue
			}
			if err := c.validateCreateAlias(&d); err != nil {
				return nil, err
			}
			names[d.name] = true
			out = append(out, &d)
		}
	}
	return out, nil
}

func (c *Cluster) validateCreateAlias(d *aliasDef) error {
	if err := validateAliasName(d.name, d.effectiveIndexRouting()); err != nil {
		return err
	}
	if other, exists := c.indices[d.name]; exists {
		return &Error{Status: http.StatusBadRequest, Type: "invalid_alias_name_exception", Reason: "Invalid alias name [" + d.name + "], an index exists with the same name as the alias", Index: other.Name}
	}
	return nil
}

// applyIndexAliases stores resolved aliases on a new index, validating their
// filters against its mapping and the write index / hidden consistency.
func (c *Cluster) applyIndexAliases(ix *Index, defs []*aliasDef) error {
	for _, d := range defs {
		if err := c.validateAliasFilter(ix, d.name, d.filter); err != nil {
			return err
		}
	}
	work := map[string]map[string]aliasEntry{ix.Name: {}}
	for _, d := range defs {
		work[ix.Name][d.name] = aliasEntry{alias: d.toAlias(), hidden: d.isHidden}
	}
	saved, existed := c.indices[ix.Name]
	c.indices[ix.Name] = ix
	var err error
	for _, d := range defs {
		if err = c.validateAliasProperties(d.name, nil, work); err != nil {
			break
		}
	}
	if existed {
		c.indices[ix.Name] = saved
	} else {
		delete(c.indices, ix.Name)
	}
	if err != nil {
		return err
	}
	for _, d := range defs {
		ix.Aliases[d.name] = d.toAlias()
	}
	return nil
}
