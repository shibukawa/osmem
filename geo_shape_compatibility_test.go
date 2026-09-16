package osmem

import "testing"

// geo_shape query, checked against OpenSearch 3.8.0 (index/80_geo_point.yml).
// The confirmed differential coverage is entirely geo_shape queries run
// against geo_point fields; there is no dedicated geo_shape test suite in
// the OpenSearch 3.8.0 core rest-api-spec to source a geo_shape-field case
// from (see query_geo_shape.go's package comment for what that path does).
func TestGeoShapeQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()

	t.Run("single point, various formats", func(t *testing.T) {
		dslIndex(t, c, "geoshape1", `{"mappings":{"properties":{"location":{"type":"geo_point"}}}}`,
			`{"location":{"lon":52.374081,"lat":4.912350}}`,
			`{"location":"4.901618,52.369219"}`,
			`{"location":[52.371667,4.914722]}`,
			`{"location":"POINT (52.371667 4.914722)"}`,
			`{"location":"t0v5zsq1gpzf"}`,
			`{"location":{"type":"Point","coordinates":[52.371667,4.914722]}}`,
		)
		ids, _, st, res := dslSearch(t, c, "geoshape1", `{"geo_shape":{"location":{"shape":{"type":"envelope","coordinates":[[51,5],[53,3]]}}}}`)
		if st != 200 || len(ids) != 6 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
		ids, _, st, res = dslSearch(t, c, "geoshape1", `{"geo_shape":{"location":{"shape":{"type":"envelope","coordinates":[[151,15],[153,13]]}}}}`)
		if st != 200 || len(ids) != 0 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
	})

	t.Run("multi points, second point falls in the far envelope", func(t *testing.T) {
		dslIndex(t, c, "geoshape2", `{"mappings":{"properties":{"location":{"type":"geo_point"}}}}`,
			`{"location":[{"lon":52.374081,"lat":4.912350},{"lon":152.374081,"lat":14.912350}]}`,
			`{"location":["4.901618,52.369219","14.901618,152.369219"]}`,
			`{"location":[[52.371667,4.914722],[152.371667,14.914722]]}`,
			`{"location":["POINT (52.371667 4.914722)","POINT (152.371667 14.914722)"]}`,
			`{"location":["t0v5zsq1gpzf","x6skg0zbhnum"]}`,
			`{"location":[{"type":"Point","coordinates":[52.371667,4.914722]},{"type":"Point","coordinates":[152.371667,14.914722]}]}`,
		)
		ids, _, st, res := dslSearch(t, c, "geoshape2", `{"geo_shape":{"location":{"shape":{"type":"envelope","coordinates":[[51,5],[53,3]]}}}}`)
		if st != 200 || len(ids) != 6 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
		ids, _, st, res = dslSearch(t, c, "geoshape2", `{"geo_shape":{"location":{"shape":{"type":"envelope","coordinates":[[151,15],[153,13]]}}}}`)
		if st != 200 || len(ids) != 6 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
	})

	t.Run("relations and shapes", func(t *testing.T) {
		dslIndex(t, c, "geoshape3", `{"mappings":{"properties":{"loc":{"type":"geo_point"},"k":{"type":"keyword"}}}}`,
			`{"loc":{"lat":10,"lon":10}}`,
			`{"loc":{"lat":50,"lon":50}}`,
		)
		runDSLCases(t, c, "geoshape3", []dslCase{
			{name: "envelope intersects", query: `{"geo_shape":{"loc":{"shape":{"type":"envelope","coordinates":[[0,20],[20,0]]}}}}`, ids: []string{"1"}},
			{name: "polygon intersects", query: `{"geo_shape":{"loc":{"shape":{"type":"polygon","coordinates":[[[0,0],[20,0],[20,20],[0,20],[0,0]]]}}}}`, ids: []string{"1"}},
			{name: "circle intersects", query: `{"geo_shape":{"loc":{"shape":{"type":"circle","coordinates":[10,10],"radius":"10km"}}}}`, ids: []string{"1"}},
			// geo_point fields only support the "intersects" relation and
			// area shapes (envelope/polygon/circle, not bare point/multipoint).
			{name: "disjoint relation rejected", query: `{"geo_shape":{"loc":{"shape":{"type":"envelope","coordinates":[[0,20],[20,0]]},"relation":"disjoint"}}}`, root: "query_shard_exception", why: "DISJOINT query relation not supported for Field [loc]."},
			{name: "point shape rejected", query: `{"geo_shape":{"loc":{"shape":{"type":"point","coordinates":[10,10]}}}}`, root: "query_shard_exception", why: "Field [loc] does not support point queries"},
			{name: "unmapped field always fails", query: `{"geo_shape":{"nope":{"shape":{"type":"envelope","coordinates":[[0,20],[20,0]]}}}}`, root: "query_shard_exception", why: "failed to find type for field [nope]"},
			{name: "wrong field type", query: `{"geo_shape":{"k":{"shape":{"type":"point","coordinates":[10,10]}}}}`, root: "query_shard_exception", why: "Field [k] is of unsupported type [keyword] for [geo_shape] query"},
		})
	})
}
