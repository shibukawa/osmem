package engine

import "strings"

// sourceFilter describes _source filtering.
type sourceFilter struct {
	disabled bool
	includes []string
	excludes []string
}

// parseSourceParam parses the "_source" body value of a search or get.
func parseSourceParam(v any) sourceFilter {
	switch t := v.(type) {
	case nil:
		return sourceFilter{}
	case bool:
		return sourceFilter{disabled: !t}
	case string:
		if t == "false" {
			return sourceFilter{disabled: true}
		}
		if t == "true" {
			return sourceFilter{}
		}
		return sourceFilter{includes: splitList(t)}
	case []any:
		var inc []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				inc = append(inc, s)
			}
		}
		return sourceFilter{includes: inc}
	case M:
		sf := sourceFilter{}
		if enabled, ok := t["enabled"].(bool); ok && !enabled {
			sf.disabled = true
		}
		sf.includes = append(sf.includes, getStrings(t, "includes")...)
		sf.includes = append(sf.includes, getStrings(t, "include")...)
		sf.excludes = append(sf.excludes, getStrings(t, "excludes")...)
		sf.excludes = append(sf.excludes, getStrings(t, "exclude")...)
		return sf
	}
	return sourceFilter{}
}

func mappingSourceFilter(m *Mapping) sourceFilter {
	if m == nil {
		return sourceFilter{}
	}
	return parseSourceParam(getMap(m.Extra, "_source"))
}

// applySourceFilters applies index-level _source filtering before any
// request-level filtering. The filters are sequential so includes intersect
// instead of becoming a broader union.
func applySourceFilters(src M, filters ...sourceFilter) (M, bool) {
	for _, sf := range filters {
		if sf.disabled {
			return nil, false
		}
		if !sf.isPlain() {
			src = sf.apply(src)
		}
	}
	return src, true
}

// sourceFilterFromParams builds a filter from URL parameters.
func sourceFilterFromParams(p Params) sourceFilter {
	sf := sourceFilter{}
	if p.Has("_source") {
		v := p.Get("_source")
		switch v {
		case "false":
			sf.disabled = true
		case "true", "":
		default:
			sf.includes = splitList(v)
		}
	}
	sf.includes = append(sf.includes, splitList(p.Get("_source_includes"))...)
	sf.includes = append(sf.includes, splitList(p.Get("_source_include"))...)
	sf.excludes = append(sf.excludes, splitList(p.Get("_source_excludes"))...)
	sf.excludes = append(sf.excludes, splitList(p.Get("_source_exclude"))...)
	return sf
}

// sourceParamsSet reports whether the URL asks for _source explicitly.
func sourceParamsSet(p Params) bool {
	return p.Has("_source") || p.Has("_source_includes") || p.Has("_source_excludes") || p.Has("_source_include") || p.Has("_source_exclude")
}

// validate rejects an entry that is both included and excluded, as
// OpenSearch 3's FetchSourceContext does.
func (sf sourceFilter) validate() error {
	for _, inc := range sf.includes {
		for _, exc := range sf.excludes {
			if inc == exc {
				return errIllegalArgument("The same entry [%s] cannot be both included and excluded in _source.", inc)
			}
		}
	}
	return nil
}

func (sf sourceFilter) isPlain() bool {
	return !sf.disabled && len(sf.includes) == 0 && len(sf.excludes) == 0
}

// apply filters a source tree. It returns nil when _source is disabled.
func (sf sourceFilter) apply(src M) M {
	if sf.disabled {
		return nil
	}
	if len(sf.includes) == 0 && len(sf.excludes) == 0 {
		return src
	}
	out, _ := filterTree(src, "", sf.includes, sf.excludes).(M)
	if out == nil {
		out = M{}
	}
	return out
}

func filterTree(v any, path string, includes, excludes []string) any {
	switch t := v.(type) {
	case M:
		out := M{}
		for k, e := range t {
			full := k
			if path != "" {
				full = path + "." + k
			}
			if matchAny(excludes, full) {
				continue
			}
			if len(includes) > 0 && !matchAny(includes, full) && !prefixOfAny(includes, full) {
				continue
			}
			if !matchAny(includes, full) && len(includes) > 0 {
				// partial match: descend
				sub := filterTree(e, full, includes, excludes)
				if sm, ok := sub.(M); ok && len(sm) == 0 {
					continue
				}
				if sub == nil {
					continue
				}
				out[k] = sub
				continue
			}
			out[k] = filterTree(e, full, nil, excludes)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			if _, ok := e.(M); ok {
				sub := filterTree(e, path, includes, excludes)
				if sm, ok := sub.(M); ok && len(sm) == 0 && len(includes) > 0 {
					continue
				}
				out = append(out, sub)
				continue
			}
			if _, ok := e.([]any); ok {
				out = append(out, filterTree(e, path, includes, excludes))
				continue
			}
			if len(includes) > 0 && !matchAny(includes, path) {
				continue
			}
			out = append(out, e)
		}
		return out
	default:
		if len(includes) > 0 && !matchAny(includes, path) {
			return nil
		}
		return v
	}
}

// prefixOfAny reports whether some pattern could match a path below prefix.
func prefixOfAny(patterns []string, prefix string) bool {
	for _, p := range patterns {
		if wildcardPrefixMatch(p, prefix+".") {
			return true
		}
	}
	return false
}

// wildcardPrefixMatch reports whether pattern can match some string that
// starts with s.
func wildcardPrefixMatch(pattern, s string) bool {
	var match func(p, s string) bool
	match = func(p, s string) bool {
		if len(s) == 0 {
			return true
		}
		if len(p) == 0 {
			return false
		}
		switch p[0] {
		case '*':
			for i := 0; i <= len(s); i++ {
				if match(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			return match(p[1:], s[1:])
		default:
			if p[0] != s[0] {
				return false
			}
			return match(p[1:], s[1:])
		}
	}
	return match(pattern, s)
}

// pathExists reports whether a dotted path is present in the tree.
func pathExists(src M, path string) bool {
	return lookupPath(src, path) != nil
}

// hasPrefixDot reports whether s starts with prefix followed by a dot.
func hasPrefixDot(s, prefix string) bool {
	return strings.HasPrefix(s, prefix+".")
}
