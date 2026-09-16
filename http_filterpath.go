package osmem

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/shibukawa/osmem/internal/engine"
)

// filter_path works like OpenSearch's filtered XContent generators
// (FilterPathBasedFilter under Jackson's FilteringGeneratorDelegate):
//
//   - the value is split on commas; entries starting with '-' exclude, the
//     others include; both kinds may be combined (includes apply first)
//   - a path is split on dots not escaped with a backslash; a segment is a
//     glob where only '*' is special, "**" matches any number of levels
//   - a fully matched path keeps (or, for exclusions, drops) the whole value,
//     null included
//   - objects and arrays on a partially matched path are written only when
//     something inside them is; for inclusions scalars there are dropped,
//     for exclusions kept
//   - the root object is always written; a root array (the JSON _cat
//     responses) disappears when nothing is left
//   - error responses are never filtered

type filterPath struct {
	include [][]string
	exclude [][]string
}

// parseFilterPath returns the filters of a filter_path value; active is
// false when the value asks for no filtering.
func parseFilterPath(raw string) (fp filterPath, active bool, err *engine.Error) {
	if raw == "" {
		return fp, false, nil
	}
	var includes, excludes []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimFunc(part, func(r rune) bool { return r <= ' ' })
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		if token[0] == '-' {
			excludes = append(excludes, token[1:])
		} else {
			includes = append(includes, token)
		}
	}
	fp.include = compileFilterPaths(includes)
	fp.exclude = compileFilterPaths(excludes)
	if (len(includes) > 0 && len(fp.include) == 0) || (len(excludes) > 0 && len(fp.exclude) == 0) {
		return fp, true, &engine.Error{Status: 400, Type: "illegal_argument_exception", Reason: "filters cannot be null or empty"}
	}
	return fp, len(fp.include) > 0 || len(fp.exclude) > 0, nil
}

// compileFilterPaths is FilterPath.compile.
func compileFilterPaths(filters []string) [][]string {
	var out [][]string
	for _, f := range filters {
		f = strings.TrimFunc(f, func(r rune) bool { return r <= ' ' })
		if f == "" {
			continue
		}
		var segments []string
		start := 0
		for i := 0; i < len(f); i++ {
			if f[i] == '.' && (i == 0 || f[i-1] != '\\') {
				segments = append(segments, f[start:i])
				start = i + 1
			}
		}
		segments = append(segments, f[start:])
		for len(segments) > 0 && segments[len(segments)-1] == "" {
			segments = segments[:len(segments)-1]
		}
		for i, s := range segments {
			segments[i] = strings.ReplaceAll(s, "\\.", ".")
		}
		out = append(out, segments)
	}
	return out
}

type filterCursor struct {
	segments []string
	index    int
}

// evaluate is FilterPathBasedFilter.evaluate for one property name.
func evaluateFilterPath(name string, cursors []filterCursor) (matched bool, next []filterCursor) {
	for _, c := range cursors {
		if c.index >= len(c.segments) {
			continue
		}
		seg := c.segments[c.index]
		if seg != "*" && seg != "**" && !globMatch(seg, name) {
			continue
		}
		if c.index+1 == len(c.segments) {
			return true, nil
		}
		if seg == "**" {
			next = append(next, c)
		}
		next = append(next, filterCursor{segments: c.segments, index: c.index + 1})
	}
	return false, next
}

func filterValue(v any, cursors []filterCursor, inclusive bool) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			matched, next := evaluateFilterPath(k, cursors)
			switch {
			case matched:
				if inclusive {
					out[k] = child
				}
			case len(next) == 0:
				if !inclusive {
					out[k] = child
				}
			default:
				if r, ok := filterValue(child, next, inclusive); ok {
					out[k] = r
				}
			}
		}
		return out, len(out) > 0
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			if r, ok := filterValue(e, cursors, inclusive); ok {
				out = append(out, r)
			}
		}
		return out, len(out) > 0
	default:
		return v, !inclusive
	}
}

// apply filters a response body. ok is false when nothing is left to write.
func (fp filterPath) apply(body any) (out any, ok bool) {
	raw, err := engine.EncodeJSON(body, false)
	if err != nil {
		return body, true
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return body, true
	}
	_, rootObject := tree.(map[string]any)
	for _, pass := range []struct {
		paths     [][]string
		inclusive bool
	}{{fp.include, true}, {fp.exclude, false}} {
		if len(pass.paths) == 0 {
			continue
		}
		cursors := make([]filterCursor, len(pass.paths))
		for i, p := range pass.paths {
			cursors[i] = filterCursor{segments: p}
		}
		filtered, kept := filterValue(tree, cursors, pass.inclusive)
		if !kept && !rootObject {
			return nil, false
		}
		tree = filtered
	}
	return tree, true
}

// globMatch is org.opensearch.core.common.regex.Glob.globMatch (which
// Regex.simpleMatch delegates to): only '*' is a wildcard. The cat APIs and
// filter_path share it.
func globMatch(pattern, str string) bool {
	first := strings.IndexByte(pattern, '*')
	if first == -1 {
		return pattern == str
	}
	if first == 0 {
		if len(pattern) == 1 {
			return true
		}
		next := strings.IndexByte(pattern[1:], '*')
		if next == -1 {
			return strings.HasSuffix(str, pattern[1:])
		}
		next++
		if next == 1 {
			return globMatch(pattern[1:], str)
		}
		part := pattern[1:next]
		for from := 0; ; {
			i := strings.Index(str[from:], part)
			if i == -1 {
				return false
			}
			i += from
			if globMatch(pattern[next:], str[i+len(part):]) {
				return true
			}
			from = i + 1
		}
	}
	return len(str) >= first && pattern[:first] == str[:first] && globMatch(pattern[first:], str[first:])
}
