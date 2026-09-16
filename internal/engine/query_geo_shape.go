package engine

import (
	"math"
	"strings"

	"github.com/blevesearch/bleve/v2/search/query"
)

// geo_shape query. The confirmed differential coverage (index/80_geo_point.yml)
// is entirely "geo_shape query against a geo_point field" with an envelope
// shape, which geoShapeAgainstPoints answers exactly (point-in-shape,
// including antimeridian-crossing envelopes and real point-in-polygon).
// A geo_shape query against an actual geo_shape-typed field is not exercised
// by any fetched OpenSearch 3.8.0 REST test (there is no dedicated geo_shape
// suite in the core rest-api-spec at this tag), and osmem does not otherwise
// index geo_shape field values into shard-level geometry yet;
// geoShapeAgainstShapes below is a best-effort, bounding-box-only relation
// check computed by scanning _source directly, not real polygon geometry.

type geoShape struct {
	kind         string // point, multipoint, envelope, polygon, circle
	points       []geoPointSpec
	radiusMeters float64
}

type geoShapeSpec struct {
	field    string
	shape    *geoShape
	relation string // intersects (default), within, contains, disjoint
}

// geo_shape's inner field object rejects any field it does not know,
// including boost, _name and ignore_unmapped (all three only work as
// siblings of the field name, one level up; ignore_unmapped is rejected in
// either position on OpenSearch 3.8, so osmem does not accept it at all),
// with a bare "query does not support [x]" (no "[geo_shape]" prefix, unlike
// most other query bodies) - all confirmed against a real 3.8.0 server.
func parseGeoShapeQuery(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &geoShapeSpec{relation: "intersects"}
	found := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if obj, isObj := v.(M); isObj {
			spec.field, found = k, true
			for _, pk := range queryKeys(obj) {
				pv := obj[pk]
				switch pk {
				case "shape":
					shape, err := parseGeoShapeGeometry(pv)
					if err != nil {
						return nil, err
					}
					spec.shape = shape
				case "relation":
					rel := strings.ToLower(xText(pv))
					switch rel {
					case "intersects", "within", "contains", "disjoint":
						spec.relation = rel
					default:
						return nil, pIllegalArgument("Unknown shape operation [%s]", rel)
					}
				case "indexed_shape":
					return nil, errUnsupported("[indexed_shape] in [geo_shape] queries")
				default:
					return nil, pParsing("query does not support [%s]", pk).at(valueTok(obj, pk))
				}
			}
			continue
		}
		if !isXValue(v) {
			continue
		}
		switch k {
		case "_name":
			n.name = xText(v)
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		}
	}
	if !found {
		return nil, pIllegalArgument("failed to find geo_shape field")
	}
	if spec.shape == nil {
		return nil, pIllegalArgument("No Shape Provided")
	}
	n.spec = spec
	return n, nil
}

// parseGeoShapeGeometry parses a GeoJSON-style shape object or a WKT string.
func parseGeoShapeGeometry(v any) (*geoShape, *Error) {
	switch t := v.(type) {
	case M:
		typ := strings.ToLower(xText(t["type"]))
		switch typ {
		case "envelope":
			coords, ok := t["coordinates"].([]any)
			if !ok || len(coords) != 2 {
				return nil, pParse("Invalid number of points (2) provided for geo_shape [envelope]")
			}
			p1, err := parseGeoJSONPosition(coords[0])
			if err != nil {
				return nil, err
			}
			p2, err := parseGeoJSONPosition(coords[1])
			if err != nil {
				return nil, err
			}
			return &geoShape{kind: "envelope", points: []geoPointSpec{p1, p2}}, nil
		case "point":
			p, err := parseGeoJSONPosition(t["coordinates"])
			if err != nil {
				return nil, err
			}
			return &geoShape{kind: "point", points: []geoPointSpec{p}}, nil
		case "multipoint":
			arr, _ := t["coordinates"].([]any)
			var pts []geoPointSpec
			for _, e := range arr {
				p, err := parseGeoJSONPosition(e)
				if err != nil {
					return nil, err
				}
				pts = append(pts, p)
			}
			return &geoShape{kind: "multipoint", points: pts}, nil
		case "polygon", "multipolygon":
			arr, _ := t["coordinates"].([]any)
			if len(arr) == 0 {
				return nil, pParse("polygon must have at least one ring")
			}
			ring := arr[0]
			if typ == "multipolygon" {
				poly, _ := arr[0].([]any)
				if len(poly) == 0 {
					return nil, pParse("polygon must have at least one ring")
				}
				ring = poly[0]
			}
			ringPts, ok := ring.([]any)
			if !ok {
				return nil, pParse("geo_point expected")
			}
			var pts []geoPointSpec
			for _, e := range ringPts {
				p, err := parseGeoJSONPosition(e)
				if err != nil {
					return nil, err
				}
				pts = append(pts, p)
			}
			return &geoShape{kind: "polygon", points: pts}, nil
		case "circle":
			p, err := parseGeoJSONPosition(t["coordinates"])
			if err != nil {
				return nil, err
			}
			meters, rerr := parseDistance(xText(t["radius"]), 1)
			if rerr != nil {
				return nil, rerr
			}
			return &geoShape{kind: "circle", points: []geoPointSpec{p}, radiusMeters: meters}, nil
		}
		return nil, pIllegalArgument("Unknown shape type [%s]", typ)
	case string:
		return parseWKTShape(t)
	}
	return nil, pParse("shape must be an object or a WKT string")
}

func parseGeoJSONPosition(v any) (geoPointSpec, *Error) {
	arr, ok := v.([]any)
	if !ok || len(arr) < 2 {
		return geoPointSpec{}, pParse("geo_point expected")
	}
	lon, ok1 := toFloat(arr[0])
	lat, ok2 := toFloat(arr[1])
	if !ok1 || !ok2 {
		return geoPointSpec{}, pParse("geo_point expected")
	}
	return geoPointSpec{lat: lat, lon: lon}, nil
}

func parseWKTShape(s string) (*geoShape, *Error) {
	trimmed := strings.TrimSpace(s)
	switch upper := strings.ToUpper(trimmed); {
	case strings.HasPrefix(upper, "POINT"):
		lat, lon, ok := geoPointValue(trimmed)
		if !ok {
			return nil, pParse("failed to parse [%s]", trimmed)
		}
		return &geoShape{kind: "point", points: []geoPointSpec{{lat: lat, lon: lon}}}, nil
	case strings.HasPrefix(upper, "BBOX"):
		env, ok := parseWKTBBox(trimmed)
		if !ok {
			return nil, pParse("failed to parse [%s]", trimmed)
		}
		return &geoShape{kind: "envelope", points: []geoPointSpec{{lat: env[2], lon: env[0]}, {lat: env[3], lon: env[1]}}}, nil
	}
	return nil, pParse("Unknown geometry type: %s", trimmed)
}

// bbox is the bounding box of a shape (a degrees-based approximation for
// circle, whose radius is converted at its center's latitude).
func (s *geoShape) bbox() (minLat, maxLat, minLon, maxLon float64) {
	if s.kind == "circle" {
		p := s.points[0]
		dLat := s.radiusMeters / 111320.0
		dLon := dLat
		if c := math.Cos(p.lat * math.Pi / 180); math.Abs(c) > 1e-9 {
			dLon = s.radiusMeters / (111320.0 * math.Abs(c))
		}
		return p.lat - dLat, p.lat + dLat, p.lon - dLon, p.lon + dLon
	}
	minLat, minLon = math.Inf(1), math.Inf(1)
	maxLat, maxLon = math.Inf(-1), math.Inf(-1)
	for _, p := range s.points {
		minLat, maxLat = math.Min(minLat, p.lat), math.Max(maxLat, p.lat)
		minLon, maxLon = math.Min(minLon, p.lon), math.Max(maxLon, p.lon)
	}
	return
}

// containsPoint is an exact point-in-shape test (point, multipoint, circle:
// exact; envelope: exact, including antimeridian crossing; polygon: exact
// ray-casting against the outer ring, holes ignored).
func (s *geoShape) containsPoint(lat, lon float64) bool {
	switch s.kind {
	case "point":
		p := s.points[0]
		return p.lat == lat && p.lon == lon
	case "multipoint":
		for _, p := range s.points {
			if p.lat == lat && p.lon == lon {
				return true
			}
		}
		return false
	case "envelope":
		tl, br := s.points[0], s.points[1]
		top, bottom := math.Max(tl.lat, br.lat), math.Min(tl.lat, br.lat)
		if lat < bottom || lat > top {
			return false
		}
		left, right := tl.lon, br.lon
		if left <= right {
			return lon >= left && lon <= right
		}
		return lon >= left || lon <= right // crosses the antimeridian
	case "circle":
		p := s.points[0]
		return arcDistanceMeters(p.lat, p.lon, lat, lon) <= s.radiusMeters
	case "polygon":
		return pointInPolygonRing(s.points, lat, lon)
	}
	return false
}

// pointInPolygonRing is the ray-casting test of a point against a polygon's
// (lon, lat) ring.
func pointInPolygonRing(ring []geoPointSpec, lat, lon float64) bool {
	n := len(ring)
	if n < 3 {
		return false
	}
	inside := false
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		pi, pj := ring[i], ring[j]
		if (pi.lat > lat) != (pj.lat > lat) {
			atX := (pj.lon-pi.lon)*(lat-pi.lat)/(pj.lat-pi.lat) + pi.lon
			if lon < atX {
				inside = !inside
			}
		}
	}
	return inside
}

// bboxRelation reports how box a relates to box b.
func bboxRelation(aMinLat, aMaxLat, aMinLon, aMaxLon, bMinLat, bMaxLat, bMinLon, bMaxLon float64) (intersects, aWithinB, aContainsB bool) {
	intersects = aMinLat <= bMaxLat && bMinLat <= aMaxLat && aMinLon <= bMaxLon && bMinLon <= aMaxLon
	aWithinB = bMinLat <= aMinLat && aMaxLat <= bMaxLat && bMinLon <= aMinLon && aMaxLon <= bMaxLon
	aContainsB = aMinLat <= bMinLat && bMaxLat <= aMaxLat && aMinLon <= bMinLon && bMaxLon <= aMaxLon
	return
}

func (qb *queryBuilder) geoShapeToQuery(spec *geoShapeSpec) (query.Query, error) {
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		// geo_shape has no ignore_unmapped support at all (see
		// parseGeoShapeQuery): an unmapped field always fails the query.
		return nil, errQueryShard("failed to find type for field [%s]", spec.field)
	}
	switch f.Type {
	case TypeGeoPoint:
		return qb.geoShapeAgainstPoints(spec)
	case TypeGeoShape:
		return qb.geoShapeAgainstShapes(spec)
	}
	return nil, errQueryShard("Field [%s] is of unsupported type [%s] for [geo_shape] query", spec.field, f.Type)
}

// geoPointSupportedShapes are the shape kinds LatLonPoint's geometry
// queries accept; a bare point or multipoint (zero-area geometry) is
// rejected, and only the "intersects" relation is supported at all - both
// confirmed against a real OpenSearch 3.8.0 server.
var geoPointSupportedShapes = map[string]bool{"envelope": true, "polygon": true, "circle": true}

// geoShapeAgainstPoints answers a geo_shape query on a geo_point field: an
// exact point-in-shape test, matching a document if any of its (possibly
// multi-valued) points falls inside the shape.
func (qb *queryBuilder) geoShapeAgainstPoints(spec *geoShapeSpec) (query.Query, error) {
	if spec.relation != "intersects" {
		return nil, errQueryShard("%s query relation not supported for Field [%s].", strings.ToUpper(spec.relation), spec.field)
	}
	if !geoPointSupportedShapes[spec.shape.kind] {
		return nil, errQueryShard("Field [%s] does not support %s queries", spec.field, spec.shape.kind)
	}
	path := qb.ix.Mapping.searchPath(spec.field)
	shape := spec.shape
	return &docFuncQuery{inner: qb.existsQuery(path), fn: func(id string, _ float64) (float64, bool) {
		for _, v := range qb.docValues(id, path) {
			if p, ok := v.([2]float64); ok && shape.containsPoint(p[0], p[1]) {
				return 1, true
			}
		}
		return 0, false
	}}, nil
}

// geoShapeAgainstShapes is a best-effort geo_shape-on-geo_shape relation:
// osmem does not index geo_shape field geometry, so this reads the raw
// GeoJSON back from _source and compares bounding boxes only (not the
// actual polygon boundary), which is exact for envelope/point shapes but
// approximate for polygons.
func (qb *queryBuilder) geoShapeAgainstShapes(spec *geoShapeSpec) (query.Query, error) {
	path := qb.ix.Mapping.searchPath(spec.field)
	qMinLat, qMaxLat, qMinLon, qMaxLon := spec.shape.bbox()
	scores := map[string]float64{}
	for id, d := range qb.ix.docs {
		if d == nil || d.Src == nil {
			continue
		}
		for _, raw := range flattenValues(lookupPath(d.Src, path)) {
			shapeVal, ok := raw.(M)
			if !ok {
				continue
			}
			docShape, err := parseGeoShapeGeometry(shapeVal)
			if err != nil {
				continue
			}
			dMinLat, dMaxLat, dMinLon, dMaxLon := docShape.bbox()
			intersects, within, contains := bboxRelation(dMinLat, dMaxLat, dMinLon, dMaxLon, qMinLat, qMaxLat, qMinLon, qMaxLon)
			var match bool
			switch spec.relation {
			case "disjoint":
				match = !intersects
			case "within":
				match = within
			case "contains":
				match = contains
			default:
				match = intersects
			}
			if match {
				scores[id] = 1
				break
			}
		}
	}
	return &presetQuery{scores: scores}, nil
}
