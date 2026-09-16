package osmem

import (
	"net/http"
	"strconv"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func TestSearchShardsBasicCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test_1", nil)

	res := mustDo(t, c, http.MethodGet, "/test_1/_search_shards?routing=foo", nil)
	if jsonAt(t, res, "/shards/0/0/index") != "test_1" {
		t.Fatalf("basic search_shards: %v", res)
	}

	nodes, _ := res["nodes"].(map[string]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes: %v", res["nodes"])
	}
	for _, n := range nodes {
		node := n.(map[string]any)
		if node["name"] == nil || node["ephemeral_id"] == nil || node["transport_address"] == nil {
			t.Fatalf("node entry: %v", node)
		}
		if _, ok := node["attributes"].(map[string]any); !ok {
			t.Fatalf("node attributes: %v", node)
		}
	}
	shard := jsonAt(t, res, "/shards/0/0").(map[string]any)
	if shard["state"] != "STARTED" || shard["primary"] != true || shard["searchOnly"] != false ||
		shard["relocating_node"] != nil || shard["shard"] != float64(0) {
		t.Fatalf("shard entry: %v", shard)
	}
	if _, ok := jsonAt(t, res, "/shards/0/0/allocation_id/id").(string); !ok {
		t.Fatalf("allocation_id: %v", shard)
	}
}

func TestSearchShardsMultiIndexAndMissingCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/s1", `{"settings":{"number_of_shards":3,"number_of_replicas":1}}`)
	mustDo(t, c, http.MethodPut, "/s2", `{"settings":{"number_of_shards":2,"number_of_replicas":0}}`)

	res := mustDo(t, c, http.MethodGet, "/s1,s2/_search_shards", nil)
	shards, _ := res["shards"].([]any)
	// replicas are unassigned on osmem's single node and excluded, so each
	// group has exactly the index's primaries, one shard per group
	if len(shards) != 5 {
		t.Fatalf("shard groups (replicas must be excluded): %v", res)
	}
	for _, g := range shards {
		group, _ := g.([]any)
		if len(group) != 1 {
			t.Fatalf("one replica-free copy per shard: %v", group)
		}
	}
	indices, _ := res["indices"].(map[string]any)
	if _, ok := indices["s1"]; !ok {
		t.Fatalf("indices.s1 missing: %v", res)
	}
	if _, ok := indices["s2"]; !ok {
		t.Fatalf("indices.s2 missing: %v", res)
	}

	// a wildcard or explicit name matching nothing answers empty, not 404
	if st, body := status(t, c, http.MethodGet, "/does_not_exist_xyz/_search_shards", nil); st != 200 {
		t.Fatalf("missing index status: %d %v", st, body)
	} else {
		assertJSON(t, body, `{"nodes":{},"indices":{},"shards":[]}`)
	}
}

func TestSearchShardsRoutingCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/rt", `{"settings":{"number_of_shards":3,"number_of_replicas":0}}`)

	all := mustDo(t, c, http.MethodGet, "/rt/_search_shards", nil)
	if shards, _ := all["shards"].([]any); len(shards) != 3 {
		t.Fatalf("all shards: %v", all)
	}

	// routing narrows the response to the shard(s) that value hashes to
	narrowed := mustDo(t, c, http.MethodGet, "/rt/_search_shards?routing=abc", nil)
	shards, _ := narrowed["shards"].([]any)
	if len(shards) == 0 || len(shards) >= 3 {
		t.Fatalf("routing should narrow shards: %v", narrowed)
	}
	shard := jsonAt(t, narrowed, "/shards/0/0/shard")
	// the same routing value always narrows to the same shard(s)
	again := mustDo(t, c, http.MethodGet, "/rt/_search_shards?routing=abc", nil)
	if jsonAt(t, again, "/shards/0/0/shard") != shard {
		t.Fatalf("routing is not stable: %v vs %v", shard, jsonAt(t, again, "/shards/0/0/shard"))
	}
}

func TestSearchShardsAliasFilterCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test_index", `{"settings":{"index":{"number_of_shards":1,"number_of_replicas":0}},
		"mappings":{"properties":{"field":{"type":"text"}}},
		"aliases":{"test_alias_no_filter":{},"test_alias_filter_1":{"filter":{"term":{"field":"value1"}}},"test_alias_filter_2":{"filter":{"term":{"field":"value2"}}}}}`)

	// an alias without a filter reports its name and no filter
	res := mustDo(t, c, http.MethodGet, "/test_alias_no_filter/_search_shards", nil)
	if jsonAt(t, res, "/shards/0/0/index") != "test_index" {
		t.Fatalf("no-filter alias shards: %v", res)
	}
	assertJSON(t, jsonAt(t, res, "/indices/test_index/aliases"), `["test_alias_no_filter"]`)
	if _, ok := jsonAt(t, res, "/indices/test_index").(map[string]any)["filter"]; ok {
		t.Fatalf("no-filter alias should not report a filter: %v", res)
	}

	// a single filtered alias echoes its filter in verbose query DSL form
	res = mustDo(t, c, http.MethodGet, "/test_alias_filter_1/_search_shards", nil)
	assertJSON(t, jsonAt(t, res, "/indices/test_index/aliases"), `["test_alias_filter_1"]`)
	assertJSON(t, jsonAt(t, res, "/indices/test_index/filter"), `{"term":{"field":{"value":"value1","boost":1.0}}}`)

	// two filtered aliases combine into a should clause with defaults filled in
	res = mustDo(t, c, http.MethodGet, "/test_alias_filter_1,test_alias_filter_2/_search_shards", nil)
	assertJSON(t, jsonAt(t, res, "/indices/test_index/aliases"), `["test_alias_filter_1","test_alias_filter_2"]`)
	assertJSON(t, jsonAt(t, res, "/indices/test_index/filter"), `{"bool":{"should":[
		{"term":{"field":{"value":"value1","boost":1.0}}},
		{"term":{"field":{"value":"value2","boost":1.0}}}],"adjust_pure_negative":true,"boost":1.0}}`)

	// a wildcard reaching a mix of filtered and unfiltered aliases drops the
	// filter entirely (an unfiltered alias means "no restriction")
	res = mustDo(t, c, http.MethodGet, "/test*/_search_shards", nil)
	assertJSON(t, jsonAt(t, res, "/indices/test_index/aliases"), `["test_alias_filter_1","test_alias_filter_2","test_alias_no_filter"]`)
	if _, ok := jsonAt(t, res, "/indices/test_index").(map[string]any)["filter"]; ok {
		t.Fatalf("mixed filtered/unfiltered access should drop the filter: %v", res)
	}
}

func TestSearchShardsSliceCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/sl", `{"settings":{"index":{"number_of_shards":7,"number_of_replicas":0}}}`)

	check := func(body string, wantShards ...float64) {
		t.Helper()
		res := mustDo(t, c, http.MethodPost, "/sl/_search_shards", body)
		shards, _ := res["shards"].([]any)
		if len(shards) != len(wantShards) {
			t.Fatalf("%s: got %d shard groups, want %d (%v)", body, len(shards), len(wantShards), res)
		}
		for i, want := range wantShards {
			if got := jsonAt(t, res, "/shards/"+strconv.Itoa(i)+"/0/shard"); got != want {
				t.Fatalf("%s: shard %d = %v, want %v", body, i, got, want)
			}
		}
	}
	check(`{"slice":{"id":0,"max":3}}`, 0, 3, 6)
	check(`{"slice":{"id":1,"max":3}}`, 1, 4)
	check(`{"slice":{"id":2,"max":3}}`, 2, 5)

	// preference narrows the candidate shards before slicing, so slicing
	// counts positions in the narrowed list, not raw shard numbers
	checkPref := func(sliceID int, wantShards ...float64) {
		t.Helper()
		res := mustDo(t, c, http.MethodPost, "/sl/_search_shards?preference=_shards:0,2,4,6", `{"slice":{"id":`+strconv.Itoa(sliceID)+`,"max":3}}`)
		shards, _ := res["shards"].([]any)
		if len(shards) != len(wantShards) {
			t.Fatalf("preferred slice %d: got %d shard groups, want %d (%v)", sliceID, len(shards), len(wantShards), res)
		}
		for i, want := range wantShards {
			if got := jsonAt(t, res, "/shards/"+strconv.Itoa(i)+"/0/shard"); got != want {
				t.Fatalf("preferred slice %d: shard %d = %v, want %v", sliceID, i, got, want)
			}
		}
	}
	checkPref(0, 0, 6)
	checkPref(1, 2)
	checkPref(2, 4)
}
