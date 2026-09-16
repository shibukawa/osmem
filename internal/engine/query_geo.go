package engine

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/geo"
	"github.com/blevesearch/bleve/v2/search/query"
)

// geo queries on geo_point fields.

type geoPointSpec struct {
	lat, lon float64
}

func javaDouble(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return javaNumberString(f, 64)
}

// geohashCell decodes the bounds of a geohash cell.
func geohashCell(h string) (minLat, maxLat, minLon, maxLon float64, err *Error) {
	minLat, maxLat, minLon, maxLon = -90, 90, -180, 180
	even := true
	for _, r := range h {
		idx := strings.IndexRune(geohashBase32, r)
		if idx < 0 {
			return 0, 0, 0, 0, pParse("unsupported symbol [%c] in geohash [%s]", r, h)
		}
		for bit := 4; bit >= 0; bit-- {
			on := idx&(1<<uint(bit)) != 0
			if even {
				mid := (minLon + maxLon) / 2
				if on {
					minLon = mid
				} else {
					maxLon = mid
				}
			} else {
				mid := (minLat + maxLat) / 2
				if on {
					minLat = mid
				} else {
					maxLat = mid
				}
			}
			even = !even
		}
	}
	return minLat, maxLat, minLon, maxLon, nil
}

func geohashPoint(h, effective string) (geoPointSpec, *Error) {
	minLat, maxLat, minLon, maxLon, err := geohashCell(h)
	if err != nil {
		return geoPointSpec{}, err
	}
	switch effective {
	case "top_left":
		return geoPointSpec{lat: maxLat, lon: minLon}, nil
	case "top_right":
		return geoPointSpec{lat: maxLat, lon: maxLon}, nil
	case "bottom_left":
		return geoPointSpec{lat: minLat, lon: minLon}, nil
	case "bottom_right":
		return geoPointSpec{lat: minLat, lon: maxLon}, nil
	}
	return geoPointSpec{lat: (minLat + maxLat) / 2, lon: (minLon + maxLon) / 2}, nil
}

// parseGeoPointAny parses a point given as an object, an array, a "lat,lon"
// string, WKT or a geohash (whose cell corner effective selects).
func parseGeoPointAny(v any, effective string) (geoPointSpec, *Error) {
	nan := math.NaN()
	switch t := v.(type) {
	case M:
		p := geoPointSpec{lat: nan, lon: nan}
		hash := ""
		for _, k := range objectKeys(t) {
			switch k {
			case "lat", "lon":
				f, ok := toFloat(t[k])
				if !ok {
					return p, withCause(pParse("[lat] and [lon] must be valid double values"), &Error{Type: "number_format_exception", Reason: javaNumberFormatReason(xText(t[k]))})
				}
				if k == "lat" {
					p.lat = f
				} else {
					p.lon = f
				}
			case "geohash":
				hash = xText(t[k])
			default:
				return p, pParse("field must be either [lat], [lon] or [geohash]")
			}
		}
		if hash != "" {
			if !math.IsNaN(p.lat) || !math.IsNaN(p.lon) {
				return p, pParse("field must be either lat/lon or geohash")
			}
			return geohashPoint(hash, effective)
		}
		return p, nil
	case []any:
		if len(t) > 3 {
			return geoPointSpec{}, pParse("[geo_point] field type does not accept > 3 dimensions")
		}
		var nums []float64
		for _, e := range t {
			f, ok := e.(float64)
			if !ok {
				n, isNum := toFloat(e)
				if _, isString := e.(string); isString || !isNum {
					return geoPointSpec{}, pParse("numeric value expected")
				}
				f = n
			}
			nums = append(nums, f)
		}
		if len(nums) < 2 {
			return geoPointSpec{}, pParse("geo_point expected")
		}
		if len(nums) == 3 {
			return geoPointSpec{}, pParse("Exception parsing coordinates: found Z value [%s] but [ignore_z_value] parameter is [false]", javaDouble(nums[2]))
		}
		return geoPointSpec{lat: nums[1], lon: nums[0]}, nil
	case string:
		s := strings.TrimSpace(t)
		if strings.HasPrefix(strings.ToUpper(s), "POINT") {
			lat, lon, ok := geoPointValue(s)
			if !ok {
				return geoPointSpec{}, pParse("failed to parse [%s]", s)
			}
			return geoPointSpec{lat: lat, lon: lon}, nil
		}
		if strings.Contains(s, ",") {
			parts := strings.Split(s, ",")
			if len(parts) != 2 && len(parts) != 3 {
				return geoPointSpec{}, pParse("failed to parse [%s], expected 2 or 3 coordinates but found: [%d]", s, len(parts))
			}
			lat, ok1 := javaDoubleOK(parts[0])
			lon, ok2 := javaDoubleOK(parts[1])
			if !ok1 || !ok2 {
				return geoPointSpec{}, pParse("[lat] and [lon] must be valid double values")
			}
			if len(parts) == 3 {
				z, _ := javaDoubleOK(parts[2])
				return geoPointSpec{}, pParse("Exception parsing coordinates: found Z value [%s] but [ignore_z_value] parameter is [false]", javaDouble(z))
			}
			return geoPointSpec{lat: lat, lon: lon}, nil
		}
		return geohashPoint(s, effective)
	}
	return geoPointSpec{}, pParse("geo_point expected")
}

var distanceUnits = []struct {
	names  []string
	meters float64
}{
	{[]string{"in", "inch"}, 0.0254},
	{[]string{"yd", "yards"}, 0.9144},
	{[]string{"ft", "feet"}, 0.3048},
	{[]string{"km", "kilometers"}, 1000},
	{[]string{"NM", "nmi", "nauticalmiles"}, 1852},
	{[]string{"mm", "millimeters"}, 0.001},
	{[]string{"cm", "centimeters"}, 0.01},
	{[]string{"mi", "miles"}, 1609.344},
	{[]string{"m", "meters"}, 1},
}

func distanceUnitMeters(name string) (float64, bool) {
	for _, u := range distanceUnits {
		for _, n := range u.names {
			if n == name {
				return u.meters, true
			}
		}
	}
	return 0, false
}

// parseDistance is DistanceUnit.parse to meters.
func parseDistance(s string, defaultMeters float64) (float64, *Error) {
	for _, u := range distanceUnits {
		for _, n := range u.names {
			if strings.HasSuffix(s, n) {
				num := s[:len(s)-len(n)]
				f, ok := javaDoubleOK(num)
				if !ok {
					if strings.TrimSpace(num) == "" {
						return 0, parseFailure(&Error{Status: 400, Type: "number_format_exception", Reason: "empty String"})
					}
					return 0, pNumberFormat(num)
				}
				return f * u.meters, nil
			}
		}
	}
	f, ok := javaDoubleOK(s)
	if !ok {
		return 0, pNumberFormat(s)
	}
	return f * defaultMeters, nil
}

func geoDistanceMeters(s string) (float64, bool) {
	m, err := parseDistance(s, 1)
	return m, err == nil
}

// arcDistanceMeters is the haversine distance OpenSearch's GeoDistance.ARC
// computes.
func arcDistanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const earthMeanRadius = 6371008.7714
	toRad := math.Pi / 180
	h1 := math.Sin((lat2 - lat1) * toRad / 2)
	h2 := math.Sin((lon2 - lon1) * toRad / 2)
	h := h1*h1 + math.Cos(lat1*toRad)*math.Cos(lat2*toRad)*h2*h2
	return 2 * earthMeanRadius * math.Asin(math.Min(1, math.Sqrt(h)))
}

func parseValidationMethod(v any) (string, *Error) {
	s := strings.ToUpper(xText(v))
	switch s {
	case "COERCE", "IGNORE_MALFORMED", "STRICT":
		return s, nil
	}
	return "", pIllegalArgument("operator needs to be either [COERCE, IGNORE_MALFORMED, STRICT], but not [%s]", xText(v))
}

// geo_distance ---------------------------------------------------------------

type geoDistanceSpec struct {
	field          string
	point          geoPointSpec
	meters         float64
	validation     string
	ignoreUnmapped bool
}

func parseGeoDistance(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &geoDistanceSpec{validation: "STRICT"}
	var distance any
	hasDistance := false
	unitMeters := 1.0
	var pointKeys []string
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "distance":
			if !isXValue(v) {
				at := noTok
				if obj, isObj := v.(M); isObj {
					// read as a point object: fails at its first member that
					// is not a coordinate
					at = pickValueTok(obj, func(order []string) (string, bool) {
						for _, name := range order {
							if name != "lat" && name != "lon" && name != "geohash" {
								return name, true
							}
						}
						return "", false
					})
				}
				return nil, pParsing("[geo_distance] query does not support [distance]").at(at)
			}
			distance, hasDistance = v, true
			continue
		case "unit":
			u, ok := distanceUnitMeters(xText(v))
			if !ok {
				return nil, pIllegalArgument("No distance unit match [%s]", xText(v))
			}
			unitMeters = u
			continue
		case "distance_type":
			switch strings.ToLower(xText(v)) {
			case "arc", "plane":
			default:
				return nil, pIllegalArgument("No geo distance for [%s]", xText(v))
			}
			continue
		case "_name":
			n.name = xText(v)
			continue
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
			continue
		case "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.ignoreUnmapped = b
			continue
		case "validation_method":
			vm, err := parseValidationMethod(v)
			if err != nil {
				return nil, err
			}
			spec.validation = vm
			continue
		}
		pointKeys = append(pointKeys, k)
	}
	// the point: an object or array value, or the first string value
	for _, k := range pointKeys {
		v := m[k]
		if spec.field != "" {
			if isObjectOrArray(v) {
				return nil, pParsing("[geo_distance] query doesn't support multiple fields, found [%s] and [%s]", spec.field, k).at(valueTok(m, k))
			}
			return nil, pParsing("failed to parse [geo_distance] query. unexpected field [%s]", k).at(valueTok(m, k))
		}
		if s, isString := v.(string); isString || isObjectOrArray(v) {
			if isString {
				if _, err := parseGeoPointAny(s, ""); err != nil && len(pointKeys) > 1 {
					continue
				}
			}
			p, err := parseGeoPointAny(v, "")
			if err != nil {
				return nil, err
			}
			spec.field, spec.point = k, p
			continue
		}
	}
	for _, k := range pointKeys {
		if k != spec.field {
			return nil, pParsing("failed to parse [geo_distance] query. unexpected field [%s]", k).at(valueTok(m, k))
		}
	}
	if !hasDistance {
		return nil, pParsing("geo_distance requires 'distance' to be specified").at(endTok(m))
	}
	var meters float64
	if s, isString := distance.(string); isString {
		d, err := parseDistance(s, unitMeters)
		if err != nil {
			return nil, err
		}
		meters = d
	} else {
		f, _ := toFloat(distance)
		meters = f * unitMeters
	}
	if meters <= 0 {
		return nil, pIllegalArgument("distance must be greater than zero")
	}
	if spec.field == "" {
		return nil, pIllegalArgument("fieldName must not be null or empty")
	}
	spec.meters = meters
	n.spec = spec
	return n, nil
}

// geoFieldCheck resolves the field of a geo query.
func (qb *queryBuilder) geoFieldCheck(kind, field string, ignoreUnmapped bool) (bool, error) {
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		if ignoreUnmapped {
			return false, nil
		}
		return false, errQueryShard("failed to find geo field [%s]", field)
	}
	if f.Type == TypeGeoShape {
		return false, errUnsupported("[" + kind + "] query on [geo_shape] fields")
	}
	if f.Type != TypeGeoPoint {
		return false, errQueryShard("type [%s] for field [%s] is not supported for [%s] queries. Must be one of [geo_point] or [geo_shape]", fieldTypeClass(f), field, kind)
	}
	return true, nil
}

func validLat(lat float64) bool { return !math.IsNaN(lat) && lat >= -90 && lat <= 90 }
func validLon(lon float64) bool { return !math.IsNaN(lon) && lon >= -180 && lon <= 180 }

func errGeoValidation(failures []string) *Error {
	var sb strings.Builder
	sb.WriteString("Validation Failed: ")
	for i, f := range failures {
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(": ")
		sb.WriteString(f)
		sb.WriteString(";")
	}
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "couldn't validate latitude/ longitude values",
		Cause: &Error{Type: "query_validation_exception", Reason: sb.String()}}
}

func centeredModulus(dividend, divisor float64) float64 {
	rtn := math.Mod(dividend, divisor)
	if rtn <= 0 {
		rtn += divisor
	}
	if rtn > divisor/2 {
		rtn -= divisor
	}
	return rtn
}

// normalizeGeoPoint is GeoUtils.normalizePoint(point, true, true).
func normalizeGeoPoint(p geoPointSpec) geoPointSpec {
	lon, lat := p.lon, p.lat
	normLat := lat > 90 || lat < -90
	normLon := lon > 180 || lon < -180 || normLat
	if normLat {
		lat = centeredModulus(lat, 360)
		shift := true
		if lat < -90 {
			lat = -180 - lat
		} else if lat > 90 {
			lat = 180 - lat
		} else {
			shift = false
		}
		if shift {
			lon += 180
		}
	}
	if normLon {
		lon = centeredModulus(lon, 360)
	}
	return geoPointSpec{lat: lat, lon: lon}
}

func (qb *queryBuilder) geoDistanceToQuery(spec *geoDistanceSpec) (query.Query, error) {
	ok, err := qb.geoFieldCheck("geo_distance", spec.field, spec.ignoreUnmapped)
	if err != nil || !ok {
		return bleve.NewMatchNoneQuery(), err
	}
	p := spec.point
	switch spec.validation {
	case "STRICT":
		var failures []string
		if !validLat(p.lat) {
			failures = append(failures, "[geo_distance] center point latitude is invalid: "+javaDouble(p.lat))
		}
		if !validLon(p.lon) {
			failures = append(failures, "[geo_distance] center point longitude is invalid: "+javaDouble(p.lon))
		}
		if len(failures) > 0 {
			return nil, errGeoValidation(failures)
		}
	case "COERCE":
		p = normalizeGeoPoint(p)
	default:
		if !validLat(p.lat) {
			return nil, errCreateQuery("illegal_argument_exception", "invalid latitude "+javaDouble(p.lat)+"; must be between -90.0 and 90.0")
		}
		if !validLon(p.lon) {
			return nil, errCreateQuery("illegal_argument_exception", "invalid longitude "+javaDouble(p.lon)+"; must be between -180.0 and 180.0")
		}
	}
	q := bleve.NewGeoDistanceQuery(p.lon, p.lat, strconv.FormatFloat(spec.meters, 'f', -1, 64)+"m")
	q.SetField(spec.field)
	return &constantScoreQuery{inner: q, score: 1}, nil
}

// geo_bounding_box -----------------------------------------------------------

type geoBBoxSpec struct {
	field                    string
	top, left, bottom, right float64
	validation               string
	ignoreUnmapped           bool
}

func parseGeoBoundingBox(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &geoBBoxSpec{validation: "STRICT"}
	found := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if box, isObj := v.(M); isObj {
			if err := parseBoundingBoxInto(box, spec); err != nil {
				return nil, parseFailure(&Error{Status: 400, Type: "parse_exception", Reason: "failed to parse [geo_bounding_box] query. [" + err.Reason + "]"})
			}
			spec.field, found = k, true
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
		case "validation_method":
			vm, err := parseValidationMethod(v)
			if err != nil {
				return nil, err
			}
			spec.validation = vm
		case "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.ignoreUnmapped = b
		case "type":
			_ = xText(v)
		default:
			return nil, pParsing("failed to parse [geo_bounding_box] query. unexpected field [%s]", k).at(valueTok(m, k))
		}
	}
	if !found {
		return nil, pParse("failed to parse [geo_bounding_box] query. bounding box not provided")
	}
	if spec.validation != "IGNORE_MALFORMED" {
		switch {
		case !isValidDouble(spec.top):
			return nil, pIllegalArgument("top latitude is invalid: %s", javaDouble(spec.top))
		case !isValidDouble(spec.left):
			return nil, pIllegalArgument("left longitude is invalid: %s", javaDouble(spec.left))
		case !isValidDouble(spec.bottom):
			return nil, pIllegalArgument("bottom latitude is invalid: %s", javaDouble(spec.bottom))
		case !isValidDouble(spec.right):
			return nil, pIllegalArgument("right longitude is invalid: %s", javaDouble(spec.right))
		case spec.top < spec.bottom:
			return nil, pIllegalArgument("top is below bottom corner: %s vs. %s", javaDouble(spec.top), javaDouble(spec.bottom))
		case spec.top == spec.bottom:
			return nil, pIllegalArgument("top cannot be the same as bottom: %s == %s", javaDouble(spec.top), javaDouble(spec.bottom))
		case spec.left == spec.right:
			return nil, pIllegalArgument("left cannot be the same as right: %s == %s", javaDouble(spec.left), javaDouble(spec.right))
		}
	}
	n.spec = spec
	return n, nil
}

func isValidDouble(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// parseBoundingBoxInto is GeoBoundingBox.parseBoundingBox; the error
// reason is the message of the parse exception.
func parseBoundingBoxInto(box M, spec *geoBBoxSpec) *Error {
	nan := math.NaN()
	spec.top, spec.left, spec.bottom, spec.right = nan, nan, nan, nan
	var envelope *[4]float64
	for _, k := range objectKeys(box) {
		v := box[k]
		switch k {
		case "wkt":
			env, ok := parseWKTBBox(xText(v))
			if !ok {
				return &Error{Reason: "failed to parse WKT bounding box"}
			}
			envelope = &env
		case "top", "bottom", "left", "right":
			f, err := xFloat(v)
			if err != nil {
				return err
			}
			switch k {
			case "top":
				spec.top = f
			case "bottom":
				spec.bottom = f
			case "left":
				spec.left = f
			default:
				spec.right = f
			}
		case "top_left", "bottom_right", "top_right", "bottom_left":
			p, err := parseGeoPointAny(v, k)
			if err != nil {
				return err
			}
			switch k {
			case "top_left":
				spec.top, spec.left = p.lat, p.lon
			case "bottom_right":
				spec.bottom, spec.right = p.lat, p.lon
			case "top_right":
				spec.top, spec.right = p.lat, p.lon
			default:
				spec.bottom, spec.left = p.lat, p.lon
			}
		default:
			return &Error{Reason: "failed to parse bounding box. unexpected field [" + k + "]"}
		}
	}
	if envelope != nil {
		if !math.IsNaN(spec.top) || !math.IsNaN(spec.bottom) || !math.IsNaN(spec.left) || !math.IsNaN(spec.right) {
			return &Error{Reason: "failed to parse bounding box. Conflicting definition found using well-known text and explicit corners."}
		}
		spec.left, spec.right, spec.top, spec.bottom = envelope[0], envelope[1], envelope[2], envelope[3]
	}
	return nil
}

// parseWKTBBox parses "BBOX (minLon, maxLon, maxLat, minLat)".
func parseWKTBBox(s string) ([4]float64, bool) {
	var out [4]float64
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToUpper(t), "BBOX") {
		return out, false
	}
	t = strings.TrimSpace(t[4:])
	if !strings.HasPrefix(t, "(") || !strings.HasSuffix(t, ")") {
		return out, false
	}
	parts := strings.Split(t[1:len(t)-1], ",")
	if len(parts) != 4 {
		return out, false
	}
	for i, p := range parts {
		f, ok := javaDoubleOK(p)
		if !ok {
			return out, false
		}
		out[i] = f
	}
	return out, true
}

func (qb *queryBuilder) geoBoundingBoxToQuery(spec *geoBBoxSpec) (query.Query, error) {
	ok, err := qb.geoFieldCheck("geo_bounding_box", spec.field, spec.ignoreUnmapped)
	if err != nil || !ok {
		return bleve.NewMatchNoneQuery(), err
	}
	top, left, bottom, right := spec.top, spec.left, spec.bottom, spec.right
	switch spec.validation {
	case "STRICT":
		var failures []string
		if !validLat(top) {
			failures = append(failures, "[geo_bounding_box] top latitude is invalid: "+javaDouble(top))
		}
		if !validLon(left) {
			failures = append(failures, "[geo_bounding_box] left longitude is invalid: "+javaDouble(left))
		}
		if !validLat(bottom) {
			failures = append(failures, "[geo_bounding_box] bottom latitude is invalid: "+javaDouble(bottom))
		}
		if !validLon(right) {
			failures = append(failures, "[geo_bounding_box] right longitude is invalid: "+javaDouble(right))
		}
		if len(failures) > 0 {
			return nil, errGeoValidation(failures)
		}
	case "COERCE":
		complete := math.Mod(right-left, 360) == 0 && right > left
		tl := normalizeGeoPoint(geoPointSpec{lat: top, lon: left})
		br := normalizeGeoPoint(geoPointSpec{lat: bottom, lon: right})
		top, left, bottom, right = tl.lat, tl.lon, br.lat, br.lon
		if complete {
			left, right = -180, 180
		}
	}
	q := bleve.NewGeoBoundingBoxQuery(left, top, right, bottom)
	q.SetField(spec.field)
	return &constantScoreQuery{inner: q, score: 1}, nil
}

// geo_polygon ----------------------------------------------------------------

type geoPolygonSpec struct {
	field          string
	points         []geoPointSpec
	validation     string
	ignoreUnmapped bool
}

func parseGeoPolygon(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &geoPolygonSpec{validation: "STRICT"}
	for _, k := range queryKeys(m) {
		v := m[k]
		if obj, isObj := v.(M); isObj {
			spec.field = k
			for _, pk := range objectKeys(obj) {
				list, isList := obj[pk].([]any)
				if !isList {
					return nil, pParsing("[geo_polygon] query does not support token type [%s] under [%s]", xTokenName(obj[pk]), pk).at(valueTok(obj, pk))
				}
				if pk != "points" {
					return nil, pParsing("[geo_polygon] query does not support [%s]", pk).at(valueTok(obj, pk))
				}
				for _, e := range list {
					p, err := parseGeoPointAny(e, "")
					if err != nil {
						return nil, err
					}
					spec.points = append(spec.points, p)
				}
			}
			continue
		}
		if !isXValue(v) {
			return nil, pParsing("[geo_polygon] unexpected token type [%s]", xTokenName(v)).at(valueTok(m, k))
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
		case "validation_method":
			vm, err := parseValidationMethod(v)
			if err != nil {
				return nil, err
			}
			spec.validation = vm
		case "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.ignoreUnmapped = b
		default:
			return nil, pParsing("[geo_polygon] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if spec.field == "" {
		return nil, pIllegalArgument("fieldName must not be null")
	}
	if len(spec.points) == 0 {
		return nil, pIllegalArgument("polygon must not be null or empty")
	}
	first, last := spec.points[0], spec.points[len(spec.points)-1]
	min := 3
	if first == last {
		min = 4
	}
	if len(spec.points) < min {
		return nil, pIllegalArgument("too few points defined for geo_polygon query")
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) geoPolygonToQuery(spec *geoPolygonSpec) (query.Query, error) {
	ok, err := qb.geoFieldCheck("geo_polygon", spec.field, spec.ignoreUnmapped)
	if err != nil || !ok {
		return bleve.NewMatchNoneQuery(), err
	}
	var points []geo.Point
	for _, p := range spec.points {
		if spec.validation != "IGNORE_MALFORMED" {
			if !validLat(p.lat) {
				return nil, errQueryShard("illegal latitude value [%s] for geo_polygon", javaDouble(p.lat))
			}
			if !validLon(p.lon) {
				return nil, errQueryShard("illegal longitude value [%s] for geo_polygon", javaDouble(p.lon))
			}
		}
		if spec.validation == "COERCE" {
			p = normalizeGeoPoint(p)
		}
		points = append(points, geo.Point{Lon: p.lon, Lat: p.lat})
	}
	if len(points) > 1 && points[0] == points[len(points)-1] {
		points = points[:len(points)-1]
	}
	q := query.NewGeoBoundingPolygonQuery(points)
	q.SetField(spec.field)
	return &constantScoreQuery{inner: q, score: 1}, nil
}
