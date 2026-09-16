package engine

import (
	"encoding/base64"
	"slices"
	"strings"
	"time"
)

// terms ----------------------------------------------------------------------

type termsSpec struct {
	field  string
	values []any
	lookup *termsLookupSpec
}

type termsLookupSpec struct {
	index, id, path, routing string
}

func parseTerms(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &termsSpec{}
	var field string
	for _, k := range queryKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case []any:
			if field != "" {
				return nil, pParsing("[terms] query does not support multiple fields").at(pickValueTok(m, termsFieldConflict(m)))
			}
			field = k
			for i, e := range t {
				switch ev := e.(type) {
				case nil:
					return nil, pParsing("No value specified for terms query").at(elemTok(t, i))
				case []any:
					// the parser reads past the nested array: to the end of
					// the field's array when it is the last element
					at := noTok
					if i == len(t)-1 {
						at = valueEndTok(m, k)
					}
					return nil, pParsing("[terms] unknown token [END_ARRAY] after [%s]", k).at(at)
				case M:
					// the parser reads the name of the object's first field
					if keys := objectKeys(ev); len(keys) > 0 {
						spec.values = append(spec.values, keys[0])
					}
				default:
					spec.values = append(spec.values, ev)
				}
			}
			if spec.values == nil {
				spec.values = []any{}
			}
		case M:
			if field != "" {
				return nil, pParsing("[terms] query does not support more than one field. Already got: [%s] but also found [%s]", field, k).at(valueTok(m, k))
			}
			field = k
			lookup, err := parseTermsLookup(t)
			if err != nil {
				return nil, err
			}
			spec.lookup = lookup
		case nil:
			return nil, pParsing("[terms] unknown token [VALUE_NULL] after [%s]", k).at(valueTok(m, k))
		default:
			switch k {
			case "boost":
				f, err := xFloat(v)
				if err != nil {
					return nil, err
				}
				n.boost = f
			case "_name":
				n.name = xText(v)
			case "value_type":
				_ = xText(v)
			default:
				return nil, pParsing("[terms] query does not support [%s]", k).at(valueTok(m, k))
			}
		}
	}
	if field == "" {
		return nil, pParsing("[terms] query requires a field name, followed by array of terms or a document lookup specification").at(endTok(m))
	}
	spec.field = field
	if n.boost < 0 {
		return nil, negativeBoostError("terms", body)
	}
	n.spec = spec
	return n, nil
}

// termsFieldConflict chooses the member at which TermsQueryBuilder finds a
// second field: the second member holding an array or an object.
func termsFieldConflict(m M) func([]string) (string, bool) {
	return func(order []string) (string, bool) {
		n := 0
		for _, k := range order {
			switch m[k].(type) {
			case []any, M:
				if n++; n == 2 {
					return k, true
				}
			}
		}
		return "", false
	}
}

func parseTermsLookup(m M) (*termsLookupSpec, *Error) {
	spec := &termsLookupSpec{}
	var hasIndex, hasPath, hasID bool
	for _, k := range objectKeys(m) {
		v := m[k]
		switch k {
		case "index":
			spec.index, hasIndex = xText(v), true
		case "id":
			spec.id, hasID = xText(v), true
		case "path":
			spec.path, hasPath = xText(v), true
		case "routing":
			spec.routing = xText(v)
		case "store":
			if _, err := xBool(v); err != nil {
				return nil, err
			}
		default:
			return nil, pXContent("[terms_lookup] unknown field [%s]", k).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	switch {
	case !hasIndex && !hasPath:
		return nil, pIllegalArgument("Required [index, path]")
	case !hasIndex:
		return nil, pIllegalArgument("Required [index]")
	case !hasPath:
		return nil, pIllegalArgument("Required [path]")
	case !hasID:
		return nil, withCause(pXContent("Failed to build [terms_lookup] after last required field arrived"),
			&Error{Type: "illegal_argument_exception", Reason: "[terms] query lookup element requires specifying either the id or the query."})
	}
	return spec, nil
}

// terms_set ------------------------------------------------------------------

type termsSetSpec struct {
	field     string
	terms     []any
	msmField  string
	msmScript bool
}

func parseTermsSet(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &termsSetSpec{}
	var field string
	for _, k := range queryKeys(m) {
		v := m[k]
		obj, ok := v.(M)
		if !ok {
			return nil, pParsing("[terms_set] query does not support [%s]", k).at(valueTok(m, k))
		}
		if field != "" {
			return nil, pParsing("[terms_set] query doesn't support multiple fields, found [%s] and [%s]", field, k).at(valueTok(m, k))
		}
		field = k
		for _, pk := range queryKeys(obj) {
			pv := obj[pk]
			switch pk {
			case "terms":
				list, isList := pv.([]any)
				if !isList {
					return nil, pParsing("[terms_set] unknown token [%s] after [terms]", xTokenName(pv)).at(valueTok(obj, pk))
				}
				spec.terms = list
			case "minimum_should_match_field":
				spec.msmField = xText(pv)
			case "minimum_should_match_script":
				spec.msmScript = true
			case "boost":
				f, err := xFloat(pv)
				if err != nil {
					return nil, err
				}
				n.boost = f
			case "_name":
				n.name = xText(pv)
			default:
				return nil, pParsing("[terms_set] query does not support [%s]", pk).at(valueTok(obj, pk))
			}
		}
	}
	spec.field = field
	n.spec = spec
	return n, nil
}

// ids ------------------------------------------------------------------------

type idsSpec struct {
	values []string
}

func parseIDs(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &idsSpec{}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "values":
			switch t := v.(type) {
			case []any:
				for i, e := range t {
					if !isXValue(e) {
						return nil, objectParserFieldError("ids", m, k, noTextAt(e, elemTok(t, i)))
					}
					spec.values = append(spec.values, xText(e))
				}
			case string, bool, float64, int:
				spec.values = append(spec.values, xText(t))
			default:
				if isXValue(t) {
					spec.values = append(spec.values, xText(t))
					continue
				}
				return nil, objectParserTypeError("ids", m, k)
			}
		case "boost":
			if _, isBool := v.(bool); isBool || !isXValue(v) {
				return nil, objectParserTypeError("ids", m, k)
			}
			f, err := xFloat(v)
			if err != nil {
				return nil, objectParserFieldError("ids", m, k, err)
			}
			if f < 0 {
				return nil, objectParserFieldError("ids", m, k, negativeBoostError("ids", body))
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			e := pXContent("[ids] unknown field [%s]%s", k, didYouMean(k, []string{"values", "boost", "_name"})).
				at(keyTok(m, k)).atParser(valueTok(m, k))
			return nil, withCause(pParsing("%s", e.Reason), e).at(valueTok(m, k)).withCauseLocation()
		}
	}
	n.spec = spec
	return n, nil
}

// exists ---------------------------------------------------------------------

type existsSpec struct {
	field string
}

func parseExists(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &existsSpec{}
	has := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if !isXValue(v) {
			return nil, unknownTokenError("exists", k, v).at(valueTok(m, k))
		}
		switch k {
		case "field":
			spec.field, has = xText(v), true
		case "_name":
			n.name = xText(v)
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		default:
			return nil, pParsing("[exists] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if !has {
		return nil, pParsing("[exists] must be provided with a [field]").at(endTok(m))
	}
	if spec.field == "" {
		return nil, pIllegalArgument("field name is null or empty")
	}
	if n.boost < 0 {
		return nil, negativeBoostError("exists", body)
	}
	n.spec = spec
	return n, nil
}

// range ----------------------------------------------------------------------

type rangeSpec struct {
	field    string
	params   M // bounds and options for rangeQuery, null bounds removed
	bounded  bool
	relation string
}

func parseRange(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &rangeSpec{}
	var field string
	for _, k := range queryKeys(m) {
		v := m[k]
		obj, ok := v.(M)
		if !ok {
			if v == nil {
				continue
			}
			return nil, pParsing("[range] query does not support [%s]", k).at(valueTok(m, k))
		}
		if field != "" {
			return nil, pParsing("[range] query doesn't support multiple fields, found [%s] and [%s]", field, k).at(valueTok(m, k))
		}
		field = k
		params := M{}
		lowerKeys, upperKeys := map[string]bool{}, map[string]bool{}
		for _, pk := range rangeParamOrder(obj) {
			pv := obj[pk]
			if inner, isObj := pv.(M); isObj {
				keys := objectKeys(inner)
				if len(keys) > 0 {
					return nil, pParsing("[range] query does not support [%s]", keys[0]).at(valueTok(inner, keys[0]))
				}
			}
			switch pk {
			case "from", "gt", "gte":
				if _, isArr := pv.([]any); isArr {
					return nil, pParsing("invalid lower bound for [range] query").at(firstInsideTok(obj, pk))
				}
				if lowerConflict(lowerKeys, pk) {
					return nil, pParsing("invalid lower bound for [range] query").at(pickValueTok(obj, rangeConflictMember(lowerBoundKeys, lowerConflict)))
				}
				lowerKeys[pk] = true
				if pv != nil {
					params[pk] = pv
					spec.bounded = true
				}
			case "to", "lt", "lte":
				if _, isArr := pv.([]any); isArr {
					return nil, pParsing("invalid upper bound for [range] query").at(firstInsideTok(obj, pk))
				}
				if upperConflict(upperKeys, pk) {
					return nil, pParsing("invalid upper bound for [range] query").at(pickValueTok(obj, rangeConflictMember(upperBoundKeys, upperConflict)))
				}
				upperKeys[pk] = true
				if pv != nil {
					params[pk] = pv
					spec.bounded = true
				}
			case "include_lower":
				if lowerConflict(lowerKeys, pk) {
					return nil, pParsing("invalid lower bound for [range] query").at(pickValueTok(obj, rangeConflictMember(lowerBoundKeys, lowerConflict)))
				}
				lowerKeys[pk] = true
				b, err := xBool(pv)
				if err != nil {
					return nil, err
				}
				params[pk] = b
			case "include_upper":
				if upperConflict(upperKeys, pk) {
					return nil, pParsing("invalid upper bound for [range] query").at(pickValueTok(obj, rangeConflictMember(upperBoundKeys, upperConflict)))
				}
				upperKeys[pk] = true
				b, err := xBool(pv)
				if err != nil {
					return nil, err
				}
				params[pk] = b
			case "boost":
				f, err := xFloat(pv)
				if err != nil {
					return nil, err
				}
				n.boost = f
			case "time_zone":
				tz := xText(pv)
				if err := checkJavaZoneID(tz); err != nil {
					return nil, err
				}
				params[pk] = tz
			case "format":
				params[pk] = xText(pv)
			case "relation":
				rel := strings.ToLower(xText(pv))
				switch rel {
				case "intersects", "within", "contains":
					spec.relation = rel
				case "disjoint":
					return nil, pIllegalArgument("[range] query does not support relation [%s]", xText(pv))
				default:
					return nil, pIllegalArgument("%s is not a valid relation", xText(pv))
				}
				params[pk] = xText(pv)
			case "_name":
				n.name = xText(pv)
			default:
				return nil, pParsing("[range] query does not support [%s]", pk).at(valueTok(obj, pk))
			}
		}
		spec.params = params
	}
	if field == "" {
		return nil, pIllegalArgument("field name is null or empty")
	}
	spec.field = field
	if n.boost < 0 {
		return nil, negativeBoostError("range", body)
	}
	n.spec = spec
	return n, nil
}

// rangeParamOrder orders the parameters of a range body; bounds first so
// that bound conflicts are reported like the request order usually has them.
func rangeParamOrder(m M) []string {
	return objectKeys(m)
}

// lowerConflict reports a second lower bound (OpenSearch 3.x rejects gt or
// gte combined with any other lower bound setting).
func lowerConflict(seen map[string]bool, key string) bool {
	if len(seen) == 0 {
		return false
	}
	if key == "gt" || key == "gte" || seen["gt"] || seen["gte"] {
		return true
	}
	return seen[key]
}

var (
	lowerBoundKeys = []string{"from", "gt", "gte", "include_lower"}
	upperBoundKeys = []string{"to", "lt", "lte", "include_upper"}
)

// rangeConflictMember chooses the bound at which a parser reading the range
// parameters in document order detects a conflicting bound.
func rangeConflictMember(bounds []string, conflict func(map[string]bool, string) bool) func([]string) (string, bool) {
	return func(order []string) (string, bool) {
		seen := map[string]bool{}
		for _, k := range order {
			if !slices.Contains(bounds, k) {
				continue
			}
			if conflict(seen, k) {
				return k, true
			}
			seen[k] = true
		}
		return "", false
	}
}

func upperConflict(seen map[string]bool, key string) bool {
	if len(seen) == 0 {
		return false
	}
	if key == "lt" || key == "lte" || seen["lt"] || seen["lte"] {
		return true
	}
	return seen[key]
}

// checkJavaZoneID validates a time zone the way ZoneId.of does.
func checkJavaZoneID(tz string) *Error {
	if len(tz) <= 1 || tz[0] == '+' || tz[0] == '-' {
		if tz == "Z" || (len(tz) > 1 && validOffset(tz)) {
			return nil
		}
		reason := "Invalid ID for ZoneOffset, invalid format: " + tz
		return withCause(pIllegalArgument("java.time.DateTimeException: %s", reason), &Error{Type: "date_time_exception", Reason: reason})
	}
	if _, err := parseTimeZone(tz); err == nil {
		return nil
	}
	for _, prefix := range []string{"UTC", "GMT", "UT"} {
		if strings.HasPrefix(tz, prefix) {
			rest := tz[len(prefix):]
			if rest == "" || validOffset(rest) {
				return nil
			}
		}
	}
	reason := "Unknown time-zone ID: " + tz
	return withCause(pIllegalArgument("java.time.zone.ZoneRulesException: %s", reason), &Error{Type: "zone_rules_exception", Reason: reason})
}

func validOffset(s string) bool {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return false
	}
	body := s[1:]
	switch len(body) {
	case 1, 2, 4, 6:
		for _, r := range body {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	case 5, 8:
		for i, r := range body {
			if (i == 2 || i == 5) && r == ':' {
				continue
			}
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// bool -----------------------------------------------------------------------

type boolSpec struct {
	must, filter, should, mustNot []*qnode
	msm                           *string
	adjustPureNegative            bool
}

var boolFields = []string{"must", "filter", "should", "must_not", "minimum_should_match", "adjust_pure_negative", "boost", "_name"}

func parseBool(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &boolSpec{adjustPureNegative: true}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "must", "filter", "should", "must_not":
			clauses, err := parseBoolClauses(m, k)
			if err != nil {
				return nil, err
			}
			switch k {
			case "must":
				spec.must = append(spec.must, clauses...)
			case "filter":
				spec.filter = append(spec.filter, clauses...)
			case "should":
				spec.should = append(spec.should, clauses...)
			default:
				spec.mustNot = append(spec.mustNot, clauses...)
			}
		case "minimum_should_match":
			switch v.(type) {
			case M, []any:
				return nil, pXContent("[bool] minimum_should_match doesn't support values of type: %s", xTokenName(v)).at(valueTok(m, k))
			}
			spec.msm = msmText(v)
		case "adjust_pure_negative":
			if !isXValue(v) {
				return nil, pXContent("[bool] adjust_pure_negative doesn't support values of type: %s", xTokenName(v)).at(valueTok(m, k))
			}
			b, err := xBool(v)
			if err != nil {
				return nil, withCause(pXContent("[bool] failed to parse field [%s]", k), err).atCause(valueEndTok(m, k))
			}
			spec.adjustPureNegative = b
		case "boost":
			if _, isBool := v.(bool); isBool || !isXValue(v) {
				return nil, pXContent("[bool] boost doesn't support values of type: %s", xTokenName(v)).at(valueTok(m, k))
			}
			f, err := xFloat(v)
			if err != nil {
				return nil, withCause(pXContent("[bool] failed to parse field [%s]", k), err).atCause(valueEndTok(m, k))
			}
			if f < 0 {
				return nil, withCause(pXContent("[bool] failed to parse field [%s]", k), negativeBoostError("bool", body)).atCause(valueEndTok(m, k))
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pXContent("[bool] unknown field [%s]%s", k, didYouMean(k, boolFields)).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	n.spec = spec
	return n, nil
}

func parseBoolClauses(m M, field string) ([]*qnode, *Error) {
	v := m[field]
	wrap := func(cause *Error) *Error {
		return withCause(pXContent("[bool] failed to parse field [%s]", field), cause).atCause(nil)
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case M:
		q, err := parseQuery(t)
		if err != nil {
			return nil, wrap(err)
		}
		return []*qnode{q}, nil
	case []any:
		out := make([]*qnode, 0, len(t))
		for i, e := range t {
			em, ok := e.(M)
			if !ok {
				// the parser moves on to the token after the element
				at := valueEndTok(m, field)
				if i+1 < len(t) {
					at = elemTok(t, i+1)
				}
				return nil, wrap(pParsing("[_na] query malformed, must start with start_object").at(at))
			}
			q, err := parseQuery(em)
			if err != nil {
				return nil, wrap(err)
			}
			out = append(out, q)
		}
		return out, nil
	}
	return nil, pXContent("[bool] %s doesn't support values of type: %s", field, xTokenName(v)).at(valueTok(m, field))
}

// constant_score -------------------------------------------------------------

type constantScoreSpec struct {
	filter *qnode
}

func parseConstantScore(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &constantScoreSpec{}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case M:
			if k != "filter" {
				return nil, pParsing("[constant_score] query does not support [%s]", k).at(valueTok(m, k))
			}
			q, err := parseQuery(t)
			if err != nil {
				return nil, err
			}
			spec.filter = q
		case []any:
			return nil, pParsing("unexpected token [START_ARRAY]").at(valueTok(m, k))
		case nil:
			return nil, pParsing("unexpected token [VALUE_NULL]").at(valueTok(m, k))
		default:
			switch k {
			case "_name":
				n.name = xText(v)
			case "boost":
				f, err := xFloat(v)
				if err != nil {
					return nil, err
				}
				n.boost = f
			default:
				return nil, pParsing("[constant_score] query does not support [%s]", k).at(valueTok(m, k))
			}
		}
	}
	if spec.filter == nil {
		return nil, pParsing("[constant_score] requires a 'filter' element").at(endTok(m))
	}
	if n.boost < 0 {
		return nil, negativeBoostError("constant_score", body)
	}
	n.spec = spec
	return n, nil
}

// dis_max --------------------------------------------------------------------

type disMaxSpec struct {
	queries []*qnode
	tie     float64
}

func parseDisMax(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &disMaxSpec{}
	found := false
	for _, k := range queryKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case M:
			if k != "queries" {
				return nil, pParsing("[dis_max] query does not support [%s]", k).at(valueTok(m, k))
			}
			found = true
			q, err := parseQuery(t)
			if err != nil {
				return nil, err
			}
			spec.queries = append(spec.queries, q)
		case []any:
			if k != "queries" {
				return nil, pParsing("[dis_max] query does not support [%s]", k).at(valueTok(m, k))
			}
			found = true
			if len(t) == 0 {
				return nil, pParsing("[_na] query malformed, must start with start_object").at(valueEndTok(m, k))
			}
			for i, e := range t {
				if _, isObj := e.(M); !isObj {
					return nil, pParsing("[_na] query malformed, must start with start_object").at(elemTok(t, i))
				}
				q, err := parseQuery(e)
				if err != nil {
					return nil, err
				}
				spec.queries = append(spec.queries, q)
			}
		default:
			switch k {
			case "boost":
				f, err := xFloat(v)
				if err != nil {
					return nil, err
				}
				n.boost = f
			case "tie_breaker":
				f, err := xFloat(v)
				if err != nil {
					return nil, err
				}
				spec.tie = f
			case "_name":
				n.name = xText(v)
			default:
				return nil, pParsing("[dis_max] query does not support [%s]", k).at(valueTok(m, k))
			}
		}
	}
	if !found {
		return nil, pParsing("[dis_max] requires 'queries' field with at least one clause").at(endTok(m))
	}
	if n.boost < 0 {
		return nil, negativeBoostError("dis_max", body)
	}
	n.spec = spec
	return n, nil
}

// boosting -------------------------------------------------------------------

type boostingSpec struct {
	positive, negative *qnode
	negativeBoost      float64
}

func parseBoosting(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &boostingSpec{negativeBoost: -1}
	for _, k := range queryKeys(m) {
		v := m[k]
		var objs []M
		switch t := v.(type) {
		case M:
			objs = []M{t}
		case []any:
			for _, e := range t {
				if em, ok := e.(M); ok {
					objs = append(objs, em)
				}
			}
		}
		if objs != nil || isObjectOrArray(v) {
			if k != "positive" && k != "negative" {
				return nil, pParsing("[boosting] query does not support [%s]", k).at(valueTok(m, k))
			}
			for _, o := range objs {
				q, err := parseQuery(o)
				if err != nil {
					return nil, err
				}
				if k == "positive" {
					spec.positive = q
				} else {
					spec.negative = q
				}
			}
			continue
		}
		switch k {
		case "negative_boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			spec.negativeBoost = f
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[boosting] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	switch {
	case spec.positive == nil:
		return nil, pParsing("[boosting] query requires 'positive' query to be set'").at(endTok(m))
	case spec.negative == nil:
		return nil, pParsing("[boosting] query requires 'negative' query to be set'").at(endTok(m))
	case spec.negativeBoost < 0:
		return nil, pParsing("[boosting] query requires 'negative_boost' to be set to be a positive value'").at(endTok(m))
	}
	if n.boost < 0 {
		return nil, negativeBoostError("boosting", body)
	}
	n.spec = spec
	return n, nil
}

func isObjectOrArray(v any) bool {
	switch v.(type) {
	case M, []any:
		return true
	}
	return false
}

// nested ---------------------------------------------------------------------

type nestedSpec struct {
	path           string
	query          *qnode
	rawQuery       M // the query body, for highlighting nested inner hits
	scoreMode      string
	ignoreUnmapped bool
	innerHits      M
	hasInnerHits   bool
}

func parseNested(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &nestedSpec{scoreMode: "avg"}
	hasPath, hasQuery := false, false
	var handle func(k string, v any, at *tokenRef) *Error
	handle = func(k string, v any, at *tokenRef) *Error {
		switch t := v.(type) {
		case []any:
			for i, e := range t {
				if err := handle(k, e, elemTok(t, i)); err != nil {
					return err
				}
			}
			return nil
		case M:
			switch k {
			case "query":
				q, err := parseQuery(t)
				if err != nil {
					return err
				}
				spec.query, spec.rawQuery, hasQuery = q, t, true
			case "inner_hits":
				spec.innerHits, spec.hasInnerHits = t, true
			default:
				return pParsing("[nested] query does not support [%s]", k).at(at)
			}
			return nil
		case nil:
			return nil
		}
		switch k {
		case "path":
			spec.path, hasPath = xText(v), true
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return err
			}
			n.boost = f
		case "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return err
			}
			spec.ignoreUnmapped = b
		case "score_mode":
			s := xText(v)
			switch s {
			case "avg", "max", "min", "none", "sum":
				spec.scoreMode = s
			default:
				return pIllegalArgument("No score mode for child query [%s] found", s)
			}
		case "_name":
			n.name = xText(v)
		default:
			return pParsing("[nested] query does not support [%s]", k).at(at)
		}
		return nil
	}
	for _, k := range queryKeys(m) {
		if err := handle(k, m[k], valueTok(m, k)); err != nil {
			return nil, err
		}
	}
	if !hasPath || spec.path == "" {
		return nil, pIllegalArgument("[nested] requires 'path' field")
	}
	if !hasQuery {
		return nil, pIllegalArgument("[nested] requires 'query' field")
	}
	if spec.hasInnerHits {
		if _, err := parseInnerHits(spec.innerHits, spec.path); err != nil {
			if e, ok := err.(*Error); ok {
				return nil, parseFailure(e)
			}
			return nil, pIllegalArgument("%v", err)
		}
	}
	if n.boost < 0 {
		return nil, negativeBoostError("nested", body)
	}
	n.spec = spec
	return n, nil
}

// wrapper --------------------------------------------------------------------

func parseWrapper(body any) (*qnode, *Error) {
	m, ok := body.(M)
	if !ok {
		return nil, pParsing("[wrapper] query malformed, no start_object after query name")
	}
	if len(m) == 0 {
		return nil, pParsing("[wrapper] query malformed").at(endTok(m))
	}
	v, has := m["query"]
	if !has {
		return nil, pParsing("[wrapper] query malformed, expected `query` but was %s", objectKeys(m)[0]).at(nthKeyTok(m, 1))
	}
	if len(m) > 1 {
		return nil, pParsing("[wrapper] malformed query, expected [END_OBJECT] but found [FIELD_NAME]").at(nthKeyTok(m, 2))
	}
	s, isString := v.(string)
	if !isString {
		reason := "Current token (" + jacksonToken(v) + ") not VALUE_STRING or VALUE_EMBEDDED_OBJECT, cannot access as binary"
		return nil, withCause(parseFailure(&Error{Status: 400, Type: "json_parse_exception", Reason: reason}), &Error{Type: "stream_read_exception", Reason: reason})
	}
	raw, err := decodeJacksonBase64(s)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, pIllegalArgument("query source text cannot be null or empty")
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return nil, pXContent("Failed to derive xcontent")
	}
	inner, derr := decodeObject(raw)
	if derr != nil {
		if e, ok := derr.(*Error); ok {
			return nil, parseFailure(e)
		}
		return nil, pParsing("%v", derr)
	}
	n, perr := parseQuery(inner)
	if perr != nil {
		// the wrapped source is parsed on its own: locations are relative to it
		LocateError(perr, raw, inner)
	}
	return n, perr
}

// decodeJacksonBase64 decodes the MIME-NO-LINEFEEDS base64 Jackson accepts
// for binary values.
func decodeJacksonBase64(s string) ([]byte, *Error) {
	var clean strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
			clean.WriteRune(r)
		default:
			reason := "Illegal character '" + string(r) + "' (code 0x" + strings.ToLower(strings.TrimLeft(hexRune(r), "0")) + ") in base64 content"
			return nil, withCause(parseFailure(&Error{Status: 400, Type: "json_parse_exception", Reason: reason}), &Error{Type: "stream_read_exception", Reason: reason})
		}
	}
	text := clean.String()
	if len(text)%4 != 0 {
		reason := "Unexpected end of base64-encoded String: base64 variant 'MIME-NO-LINEFEEDS' expects padding (one or more '=' characters) at the end. This Base64Variant might have been incorrectly configured"
		return nil, withCause(parseFailure(&Error{Status: 400, Type: "json_parse_exception", Reason: reason}), &Error{Type: "stream_read_exception", Reason: reason})
	}
	out, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		reason := "Illegal character '=' (code 0x3d) in base64 content"
		return nil, withCause(parseFailure(&Error{Status: 400, Type: "json_parse_exception", Reason: reason}), &Error{Type: "stream_read_exception", Reason: reason})
	}
	return out, nil
}

func hexRune(r rune) string {
	const digits = "0123456789ABCDEF"
	var b []byte
	for i := 3; i >= 0; i-- {
		b = append(b, digits[(r>>(uint(i)*4))&0xf])
	}
	return string(b)
}

// timeValueMillis parses an OpenSearch time value ("10d", "1h") in
// milliseconds.
func timeValueMillis(s string) (float64, bool) {
	units := []struct {
		suffix string
		millis float64
	}{{"nanos", 1e-6}, {"micros", 1e-3}, {"ms", 1}, {"s", 1000}, {"m", 60000}, {"h", 3600000}, {"d", 86400000}}
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			num := strings.TrimSpace(lower[:len(lower)-len(u.suffix)])
			if f, ok := javaDoubleOK(num); ok {
				return f * u.millis, true
			}
			return 0, false
		}
	}
	if d, err := time.ParseDuration(lower); err == nil {
		return float64(d.Milliseconds()), true
	}
	return 0, false
}
