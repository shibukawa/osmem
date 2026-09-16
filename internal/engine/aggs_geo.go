package engine

import (
	"math"
	"sort"
	"strconv"
)

// geo aggregations: geo_bounds, geo_centroid, geohash_grid, geotile_grid ---------------

type geoMetricSpec struct {
	vs   vsConfig
	wrap bool
}

func parseGeoMetric(ps *aggParser, d *aggDef) error {
	extra := map[string]int{}
	if d.kind == "geo_bounds" {
		extra["wrap_longitude"] = vtBool
	}
	of := valuesSourceFields(d.kind, false, false, extra)
	if err := of.check(d.body); err != nil {
		return err
	}
	spec := &geoMetricSpec{wrap: true}
	var err error
	if spec.vs, err = parseVSConfig(of, d.body); err != nil {
		return err
	}
	if _, ok := d.body["wrap_longitude"]; ok {
		if spec.wrap, err = of.boolValue(d.body, "wrap_longitude"); err != nil {
			return err
		}
	}
	if err := requireFieldOrScript(d.body); err != nil {
		return err
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

func prepareGeoMetric(pc *prepareCtx, d *aggDef) error {
	_, err := pc.resolve(d, 0, &d.spec.(*geoMetricSpec).vs, d.kind, vsGeoPoint, vsGeoPoint)
	return err
}

func collectGeoBounds(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*geoMetricSpec)
	top, bottom := math.Inf(-1), math.Inf(1)
	posLeft, posRight, negLeft, negRight := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		for _, p := range vs.points(h) {
			lat, lon := p[0], p[1]
			top = math.Max(top, lat)
			bottom = math.Min(bottom, lat)
			if lon >= 0 && lon < posLeft {
				posLeft = lon
			}
			if lon >= 0 && lon > posRight {
				posRight = lon
			}
			if lon < 0 && lon < negLeft {
				negLeft = lon
			}
			if lon < 0 && lon > negRight {
				negRight = lon
			}
		}
	}
	fields := M{}
	if !math.IsInf(top, 0) {
		var tl, br [2]float64
		switch {
		case math.IsInf(posLeft, 0):
			tl, br = [2]float64{top, negLeft}, [2]float64{bottom, negRight}
		case math.IsInf(negLeft, 0):
			tl, br = [2]float64{top, posLeft}, [2]float64{bottom, posRight}
		case spec.wrap:
			unwrapped := posRight - negLeft
			wrapped := (180 - posLeft) - (-180 - negRight)
			if unwrapped <= wrapped {
				tl, br = [2]float64{top, negLeft}, [2]float64{bottom, posRight}
			} else {
				tl, br = [2]float64{top, posLeft}, [2]float64{bottom, negRight}
			}
		default:
			tl, br = [2]float64{top, negLeft}, [2]float64{bottom, posRight}
		}
		fields["bounds"] = M{"top_left": M{"lat": tl[0], "lon": tl[1]}, "bottom_right": M{"lat": br[0], "lon": br[1]}}
	}
	return &aggResult{kind: resOther, fields: fields, javaClass: "InternalGeoBounds"}, nil
}

func collectGeoCentroid(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	type acc struct {
		lat, lon compensatedSum
		count    int64
	}
	shards := map[*Index]*acc{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		pts := vs.points(h)
		if len(pts) == 0 {
			continue
		}
		a := shards[h.ix]
		if a == nil {
			a = &acc{}
			shards[h.ix] = a
		}
		for _, p := range pts {
			a.lat.add(p[0])
			a.lon.add(p[1])
		}
		a.count += int64(len(pts))
	}
	latSum, lonSum := math.NaN(), math.NaN()
	var total int64
	for _, ix := range ac.indices {
		a := shards[ix]
		if a == nil || a.count == 0 {
			continue
		}
		clat, clon := a.lat.value/float64(a.count), a.lon.value/float64(a.count)
		total += a.count
		if math.IsNaN(lonSum) {
			latSum, lonSum = float64(float64(a.count)*clat), float64(float64(a.count)*clon)
		} else {
			latSum += float64(float64(a.count) * clat)
			lonSum += float64(float64(a.count) * clon)
		}
	}
	fields := M{"count": total}
	if !math.IsNaN(lonSum) {
		fields["location"] = M{"lat": latSum / float64(total), "lon": lonSum / float64(total)}
	}
	return &aggResult{kind: resOther, fields: fields, javaClass: "InternalGeoCentroid"}, nil
}

// grids --------------------------------------------------------------------------------------------

type geoGridSpec struct {
	vs        vsConfig
	precision int
	size      int
}

func parseGeoGrid(ps *aggParser, d *aggDef) error {
	of := objFields{name: d.kind, fields: map[string]int{"field": vtString, "precision": vtNumber, "size": vtNumber, "shard_size": vtNumber, "bounds": vtObject}}
	if err := of.check(d.body); err != nil {
		return err
	}
	spec := &geoGridSpec{precision: 5, size: 10000}
	if d.kind == "geotile_grid" {
		spec.precision = 7
	}
	var err error
	if spec.vs, err = parseVSConfig(of, d.body); err != nil {
		return err
	}
	if v, ok := d.body["precision"]; ok {
		if s, isStr := v.(string); isStr && d.kind == "geohash_grid" {
			if _, perr := strconv.Atoi(s); perr != nil {
				return errUnsupported("distance precision of [geohash_grid] aggregation")
			}
		}
		n, err := of.intValue(d.body, "precision")
		if err != nil {
			return err
		}
		if d.kind == "geohash_grid" && (n < 1 || n > 12) {
			return of.failed(d.body, "precision", errIllegalArgument("Invalid geohash aggregation precision of %d. Must be between 1 and 12.", n))
		}
		if d.kind == "geotile_grid" && (n < 0 || n > 29) {
			return of.failed(d.body, "precision", errIllegalArgument("Invalid geotile_grid precision of %d. Must be between 0 and 29.", n))
		}
		spec.precision = n
	}
	for _, k := range []string{"size", "shard_size"} {
		if _, ok := d.body[k]; !ok {
			continue
		}
		n, err := of.intValue(d.body, k)
		if err != nil {
			return err
		}
		if n <= 0 {
			name := k
			if k == "shard_size" {
				name = "shardSize"
			}
			return of.failed(d.body, k, errIllegalArgument("[%s] must be greater than 0. Found [%d] in [%s]", name, n, d.name))
		}
		if k == "size" {
			spec.size = n
		}
	}
	if _, ok := d.body["bounds"]; ok {
		return errUnsupported("[bounds] of [" + d.kind + "] aggregation")
	}
	if !spec.vs.hasField {
		return errIllegalArgument("Required one of fields [field, script], but none were specified. ")
	}
	d.spec = spec
	return nil
}

func prepareGeoGrid(pc *prepareCtx, d *aggDef) error {
	_, err := pc.resolve(d, 0, &d.spec.(*geoGridSpec).vs, d.kind, vsGeoPoint, vsGeoPoint)
	return err
}

func collectGeoGrid(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*geoGridSpec)
	type cell struct {
		hash int64
		b    *bucket
	}
	cells := map[int64]*cell{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		var hashes []int64
		for _, p := range vs.points(h) {
			if d.kind == "geohash_grid" {
				hashes = append(hashes, geohashLong(p[0], p[1], spec.precision))
			} else {
				hashes = append(hashes, geotileLong(p[0], p[1], spec.precision))
			}
		}
		sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
		for i, hv := range hashes {
			if i > 0 && hv == hashes[i-1] {
				continue
			}
			c, ok := cells[hv]
			if !ok {
				c = &cell{hash: hv, b: &bucket{}}
				cells[hv] = c
			}
			c.b.docCount++
			c.b.hits = append(c.b.hits, h)
		}
	}
	list := make([]*cell, 0, len(cells))
	for _, c := range cells {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].b.docCount != list[j].b.docCount {
			return list[i].b.docCount > list[j].b.docCount
		}
		return list[i].hash > list[j].hash
	})
	if len(list) > spec.size {
		list = list[:spec.size]
	}
	buckets := make([]*bucket, len(list))
	for i, c := range list {
		key := geotileString(c.hash)
		if d.kind == "geohash_grid" {
			key = geohashString(c.hash)
		}
		c.b.key, c.b.keyString = key, key
		buckets[i] = c.b
	}
	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	class := "InternalGeoTileGrid"
	if d.kind == "geohash_grid" {
		class = "InternalGeoHashGrid"
	}
	return &aggResult{kind: resBuckets, buckets: buckets, javaClass: class}, nil
}

func encodeLat(lat float64) int32 {
	if lat == 90 {
		return math.MaxInt32
	}
	return int32(math.Floor(lat / latDecode))
}

func encodeLon(lon float64) int32 {
	if lon == 180 {
		return math.MaxInt32
	}
	return int32(math.Floor(lon / lonDecode))
}

// interleave is Lucene's BitUtil.interleave: odd bits above even bits.
func interleave(even, odd uint32) uint64 {
	var out uint64
	for i := 0; i < 32; i++ {
		out |= uint64(even>>i&1) << (2 * i)
		out |= uint64(odd>>i&1) << (2*i + 1)
	}
	return out
}

// geohashLong is Geohash.longEncode.
func geohashLong(lat, lon float64, level int) int64 {
	latEnc := uint32(encodeLat(lat)) ^ 0x80000000
	lonEnc := uint32(encodeLon(lon)) ^ 0x80000000
	morton := interleave(lonEnc, latEnc) >> 2
	msf := uint((12-level)*5 + 2)
	return int64((morton>>msf)<<4) | int64(level)
}

// geohashString is Geohash.stringEncode.
func geohashString(hash int64) string {
	level := int(hash & 15)
	v := uint64(hash) >> 4
	chars := make([]byte, level)
	for level > 0 {
		level--
		chars[level] = geohashBase32[v&31]
		v >>= 5
	}
	return string(chars)
}

// geotileLong is GeoTileUtils.longEncode.
func geotileLong(lat, lon float64, precision int) int64 {
	tiles := int64(1) << uint(precision)
	x := int64(math.Floor((lon + 180) / 360 * float64(tiles)))
	latSin := math.Sin(float64(lat * 0.017453292519943295))
	y := int64(math.Floor((0.5 - (math.Log((1+latSin)/(1-latSin)) / (4 * math.Pi))) * float64(tiles)))
	if x < 0 {
		x = 0
	}
	if x >= tiles {
		x = tiles - 1
	}
	if y < 0 {
		y = 0
	}
	if y >= tiles {
		y = tiles - 1
	}
	return int64(precision)<<58 | x<<29 | y
}

func geotileString(hash int64) string {
	precision := hash >> 58
	x := hash >> 29 & (1<<29 - 1)
	y := hash & (1<<29 - 1)
	return strconv.FormatInt(precision, 10) + "/" + strconv.FormatInt(x, 10) + "/" + strconv.FormatInt(y, 10)
}

func geotileKey(lat, lon float64, precision int) string {
	return geotileString(geotileLong(lat, lon, precision))
}
