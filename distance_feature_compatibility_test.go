package osmem

import (
	"math"
	"testing"
)

// distance_feature, checked against OpenSearch 3.8.0 (search/250_distance_feature.yml
// plus scores read from that server's _explain output, which also showed
// that a date origin without sub-second precision round-up-parses like a
// range query's upper bound: "...T08:00:30Z" resolves to "...:30.999").
func TestDistanceFeatureCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "df", `{"mappings":{"properties":{
		"my_date":{"type":"date"},
		"my_date_nanos":{"type":"date_nanos"},
		"my_geo":{"type":"geo_point"},
		"k":{"type":"keyword"}
	}}}`,
		`{"my_date":"2018-02-01T10:00:00Z","my_date_nanos":"2018-02-01T00:00:00.223456789Z","my_geo":[-71.34,41.13]}`,
		`{"my_date":"2018-02-01T11:00:00Z","my_date_nanos":"2018-02-01T00:00:00.123456789Z","my_geo":[-71.34,41.14]}`,
		`{"my_date":"2018-02-01T09:00:00Z","my_date_nanos":"2018-02-01T00:00:00.323456789Z","my_geo":[-71.34,41.12]}`,
	)
	scores := func(query string) map[string]float64 {
		t.Helper()
		_, hits, st, res := dslSearch(t, c, "df", query)
		if st != 200 {
			t.Fatalf("status=%d body=%v", st, res)
		}
		out := map[string]float64{}
		for _, h := range hits {
			hm := h.(map[string]any)
			f, _ := toFloatValue(hm["_score"])
			out[hm["_id"].(string)] = f
		}
		return out
	}
	check := func(name, query string, want map[string]float64) {
		t.Run(name, func(t *testing.T) {
			got := scores(query)
			if len(got) != len(want) {
				t.Fatalf("scores = %v, want %v", got, want)
			}
			for id, w := range want {
				if math.Abs(got[id]-w) > 1e-4 {
					t.Fatalf("scores = %v, want %v", got, want)
				}
			}
		})
	}
	check("date", `{"distance_feature":{"field":"my_date","pivot":"1h","origin":"2018-02-01T08:00:30Z"}}`,
		map[string]float64{"1": 0.33429286, "2": 0.25053933, "3": 0.50216204})
	check("date_nanos", `{"distance_feature":{"field":"my_date_nanos","pivot":"100000000nanos","origin":"2018-02-01T00:00:00.323456789Z"}}`,
		map[string]float64{"1": 0.5, "2": 0.33333334, "3": 1.0})
	check("geo_point", `{"distance_feature":{"field":"my_geo","pivot":"1km","origin":[-71.35,41.12]}}`,
		map[string]float64{"1": 0.41803837, "2": 0.29617485, "3": 0.54416835})
	check("boost", `{"distance_feature":{"field":"my_date","pivot":"1h","origin":"2018-02-01T08:00:30Z","boost":2}}`,
		map[string]float64{"1": 0.6685857, "2": 0.50107867, "3": 1.0043241})

	t.Run("wrong field type", func(t *testing.T) {
		_, _, st, res := dslSearch(t, c, "df", `{"distance_feature":{"field":"k","pivot":"1km","origin":[1,1]}}`)
		typ, reason := rootCause(res)
		if st != 400 || typ != "query_shard_exception" || reason != "failed to create query: Illegal data type of [keyword]![distance_feature] query can only be run on a date, date_nanos or geo_point field type!" {
			t.Fatalf("status=%d root=%s reason=%q body=%v", st, typ, reason, res)
		}
	})
}
