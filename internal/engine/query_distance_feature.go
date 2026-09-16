package engine

import (
	"math"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// distance_feature: boosts documents by how close a date, date_nanos or
// geo_point field is to an origin (score = pivot / (pivot + distance), a
// Lucene DistanceFeatureQuery). Verified against a real OpenSearch 3.8.0
// server via _explain: for date/date_nanos fields, the origin date-math
// expression is parsed with round-up (an origin of "...T08:00:30Z" resolves
// to ...:30.999, matching the rounding a range query's upper bound gets),
// which qb.dateQueryBound(..., roundUp=true, ...) already implements.

type distanceFeatureSpec struct {
	field  string
	pivot  any
	origin any
}

func parseDistanceFeature(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &distanceFeatureSpec{}
	hasField, hasPivot, hasOrigin := false, false, false
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "field":
			spec.field, hasField = xText(v), true
		case "pivot":
			spec.pivot, hasPivot = v, true
		case "origin":
			spec.origin, hasOrigin = v, true
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pXContent("[distance_feature] unknown field [%s]", k)
		}
	}
	if !hasField {
		return nil, pIllegalArgument("Required [field]")
	}
	if !hasPivot {
		return nil, pIllegalArgument("Required [pivot]")
	}
	if !hasOrigin {
		return nil, pIllegalArgument("Required [origin]")
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) distanceFeatureToQuery(spec *distanceFeatureSpec) (query.Query, error) {
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	var pivot float64
	var distance func(id string) (float64, bool)
	switch {
	case f.Type == TypeDate || f.Type == TypeDateNanos:
		d, err := ParseTimeValue("pivot", xText(spec.pivot))
		if err != nil {
			return nil, err
		}
		origin, derr := qb.dateQueryBound(f, f.Format, spec.origin, true, time.UTC)
		if derr != nil {
			return nil, derr
		}
		field := spec.field
		if f.Type == TypeDateNanos {
			pivot = float64(d.Nanoseconds())
			originNanos := origin.UnixNano()
			distance = func(id string) (float64, bool) {
				for _, v := range qb.docValues(id, field) {
					if t, ok := v.(time.Time); ok {
						return math.Abs(float64(t.UnixNano() - originNanos)), true
					}
				}
				return 0, false
			}
		} else {
			pivot = float64(d.Milliseconds())
			originMillis := epochMillis(origin)
			distance = func(id string) (float64, bool) {
				for _, v := range qb.docValues(id, field) {
					if t, ok := v.(time.Time); ok {
						return math.Abs(float64(epochMillis(t) - originMillis)), true
					}
				}
				return 0, false
			}
		}
	case f.Type == TypeGeoPoint:
		meters, err := parseDistance(xText(spec.pivot), 1)
		if err != nil {
			return nil, err
		}
		origin, perr := parseGeoPointAny(spec.origin, "")
		if perr != nil {
			return nil, perr
		}
		pivot = meters
		field := spec.field
		distance = func(id string) (float64, bool) {
			for _, v := range qb.docValues(id, field) {
				if p, ok := v.([2]float64); ok {
					return arcDistanceMeters(origin.lat, origin.lon, p[0], p[1]), true
				}
			}
			return 0, false
		}
	default:
		return nil, errCreateQuery("illegal_argument_exception",
			"Illegal data type of ["+f.Type+"]![distance_feature] query can only be run on a date, date_nanos or geo_point field type!")
	}
	path := qb.ix.Mapping.searchPath(spec.field)
	return &docFuncQuery{inner: qb.existsQuery(path), fn: func(id string, _ float64) (float64, bool) {
		d, ok := distance(id)
		if !ok {
			return 0, false
		}
		return pivot / (pivot + d), true
	}}, nil
}
