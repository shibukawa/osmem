package engine

import (
	"strconv"
	"sync"
)

// completion context suggesters -------------------------------------------
//
// A completion field can declare "contexts": [{"name", "type": "category" |
// "geo", "path", "precision"}]. Once it does, both indexing into the field
// and querying it with a completion suggester must resolve at least one
// declared dimension to a non-empty value (CompletionFieldMapper.parse,
// ContextMappings.parseContext / SuggestionContext): an entry's own inline
// "contexts" map, falling back to the mapping's "path" against the document
// root for dimensions it does not mention. A dimension that IS mentioned
// (inline, or in a query) must resolve to at least one value.

type completionContextDef struct {
	name      string
	geo       bool
	path      string
	precision int // geohash character length (geo only)
}

// completionContexts parses the "contexts" mapping parameter of a
// completion field (stored verbatim in f.Extra by the generic "pkAny"
// mapping parameter).
func completionContexts(f *Field) []completionContextDef {
	raw, _ := f.Extra["contexts"].([]any)
	if len(raw) == 0 {
		return nil
	}
	out := make([]completionContextDef, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(M)
		if !ok {
			continue
		}
		def := completionContextDef{name: getString(m, "name"), path: getString(m, "path")}
		if getString(m, "type") == "geo" {
			def.geo = true
			def.precision = geoContextPrecision(m["precision"])
		}
		out = append(out, def)
	}
	return out
}

// geoContextPrecision resolves the "precision" of a geo context to a
// geohash character length (1-12): a raw level, or a distance (e.g. "5km")
// converted through the same cell-size table as geohash_grid. Defaults to
// 6, as OpenSearch does.
func geoContextPrecision(v any) int {
	const def = 6
	switch t := v.(type) {
	case nil:
		return def
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return clampGeohashLevel(n)
		}
		if meters, err := parseDistance(t, 1); err == nil {
			return geohashLevelForMeters(meters)
		}
	default:
		if n, ok := toFloat(t); ok {
			return clampGeohashLevel(int(n))
		}
	}
	return def
}

func clampGeohashLevel(n int) int {
	if n < 1 {
		return 1
	}
	if n > 12 {
		return 12
	}
	return n
}

// geohashCellMeters is the approximate width of a geohash cell at each
// character length (1-12).
var geohashCellMeters = [...]float64{
	5009400, 1252300, 156500, 39100, 4900, 1225, 152.9, 38.2, 4.78, 1.19, 0.149, 0.0372,
}

// geohashLevelForMeters picks the coarsest geohash level whose cell is no
// larger than the requested precision.
func geohashLevelForMeters(meters float64) int {
	for i, cell := range geohashCellMeters {
		if cell <= meters {
			return i + 1
		}
	}
	return 12
}

// isEmptyContextValue reports whether a context value given on a document
// entry or in a query counts as "no value" (an explicit empty list).
func isEmptyContextValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	}
	return false
}

// toStringList normalizes a category context value (a string or list of
// strings) to a list.
func toStringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// geoPointList normalizes a geo context value to a list of points: a single
// point (object, "lat,lon" string, geohash or [lon,lat] pair) or a list of
// them.
func geoPointList(v any) []geoPointSpec {
	var raw []any
	if arr, ok := v.([]any); ok {
		if len(arr) == 2 {
			if _, ok := toFloat(arr[0]); ok {
				if _, ok2 := toFloat(arr[1]); ok2 {
					raw = []any{v} // [lon, lat]
				}
			}
		}
		if raw == nil {
			raw = arr
		}
	} else {
		raw = []any{v}
	}
	var out []geoPointSpec
	for _, e := range raw {
		if p, err := parseGeoPointAny(e, "geo"); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// completionMandatoryFailures marks the *Error values checkMandatoryContexts
// raises: unlike addCompletion's other validation errors, OpenSearch does
// not wrap this one in a "failed to parse" mapper_parsing_exception (it
// surfaces IllegalArgumentException directly), so addCompletion's callers
// check this before wrapping their own failures.
var completionMandatoryFailures = struct {
	sync.Mutex
	m map[*Error]struct{}
}{m: map[*Error]struct{}{}}

func markCompletionMandatoryFailure(e *Error) *Error {
	completionMandatoryFailures.Lock()
	completionMandatoryFailures.m[e] = struct{}{}
	completionMandatoryFailures.Unlock()
	return e
}

// wrapCompletionFailure is errFailedToParse for addCompletion's per-value
// errors, except a mandatory-contexts failure, which is returned unwrapped.
func wrapCompletionFailure(err error) error {
	if e, ok := err.(*Error); ok {
		completionMandatoryFailures.Lock()
		_, marked := completionMandatoryFailures.m[e]
		completionMandatoryFailures.Unlock()
		if marked {
			return e
		}
	}
	return errFailedToParse(err.(*Error))
}

// checkMandatoryContexts is CompletionFieldMapper.parse validating one
// completion entry's context coverage while indexing: inline is the
// entry's own "contexts" map (nil when it gave none).
func (b *docBuilder) checkMandatoryContexts(name string, defs []completionContextDef, inline M) *Error {
	if len(defs) == 0 {
		return nil
	}
	fail := func() *Error {
		return markCompletionMandatoryFailure(errIllegalArgument("Contexts are mandatory in context enabled completion field [%s]", name))
	}
	covered := false
	for _, d := range defs {
		if inline != nil {
			if v, has := inline[d.name]; has {
				if isEmptyContextValue(v) {
					return fail()
				}
				covered = true
				continue
			}
		}
		if d.path != "" {
			if v, found := lookupPathFound(b.src.Src, d.path); found && !isEmptyContextValue(v) {
				covered = true
			}
		}
	}
	if !covered {
		return fail()
	}
	return nil
}

// contextQuery is one resolved dimension filter of a completion query's
// "contexts".
type contextQuery struct {
	categories map[string]bool
	geohashes  map[string]bool
}

// parseContextQuery validates and resolves a completion suggester's
// "contexts" against the field's declared dimensions
// (ContextMappings#parseContext / SuggestionContext): when the field
// declares any dimension, the query must give a non-empty contexts object
// covering at least one of them, and any dimension it does mention must
// resolve to 1+ values.
func parseContextQuery(defs []completionContextDef, given M) (map[string]*contextQuery, *Error) {
	if len(defs) == 0 {
		return nil, nil
	}
	out := map[string]*contextQuery{}
	covered := false
	for _, d := range defs {
		v, has := given[d.name]
		if !has {
			continue
		}
		if isEmptyContextValue(v) {
			return nil, errIllegalArgument("Missing mandatory contexts in context query")
		}
		cq := &contextQuery{}
		if d.geo {
			cq.geohashes = map[string]bool{}
			for _, p := range geoPointList(v) {
				cq.geohashes[geohashString(geohashLong(p.lat, p.lon, d.precision))] = true
			}
			if len(cq.geohashes) == 0 {
				return nil, errIllegalArgument("Missing mandatory contexts in context query")
			}
		} else {
			cq.categories = map[string]bool{}
			for _, s := range toStringList(v) {
				cq.categories[s] = true
			}
			if len(cq.categories) == 0 {
				return nil, errIllegalArgument("Missing mandatory contexts in context query")
			}
		}
		out[d.name] = cq
		covered = true
	}
	if !covered {
		return nil, errIllegalArgument("Missing mandatory contexts in context query")
	}
	return out, nil
}

// matchesContextQuery reports whether a completion entry (its own inline
// "contexts", falling back to the field's path against the document root)
// satisfies a parsed context query. A nil query (no contexts declared, or
// none given on an optional field) matches everything.
func matchesContextQuery(defs []completionContextDef, query map[string]*contextQuery, entryContexts M, doc *Doc) bool {
	for name, cq := range query {
		var def completionContextDef
		for _, dd := range defs {
			if dd.name == name {
				def = dd
				break
			}
		}
		var raw any
		has := false
		if entryContexts != nil {
			if v, ok := entryContexts[name]; ok {
				raw, has = v, true
			}
		}
		if !has && def.path != "" {
			if v, found := lookupPathFound(doc.Src, def.path); found {
				raw, has = v, true
			}
		}
		if !has {
			return false
		}
		matched := false
		if def.geo {
			for _, p := range geoPointList(raw) {
				if cq.geohashes[geohashString(geohashLong(p.lat, p.lon, def.precision))] {
					matched = true
					break
				}
			}
		} else {
			for _, s := range toStringList(raw) {
				if cq.categories[s] {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
