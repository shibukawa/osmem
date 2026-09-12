package osmem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestOpenSearchGoClient(t *testing.T) {
	base := seedCluster(t)
	defer base.Close()
	c := base.Clone()
	defer c.Close()
	srv := c.MustServe()
	defer srv.Close()
	ctx := context.Background()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Info(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version.Distribution != "opensearch" {
		t.Fatalf("info %+v", info)
	}
	// index lifecycle
	if _, err := client.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: "orders", Body: strings.NewReader(`{"mappings": {"properties": {"total": {"type": "double"}, "customer": {"type": "keyword"}, "placed": {"type": "date"}}}}`)}); err != nil {
		t.Fatal(err)
	}
	exists, err := client.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{Indices: []string{"orders"}})
	if err != nil || exists.StatusCode != 200 {
		t.Fatalf("exists %v %v", exists, err)
	}
	// documents
	docRes, err := client.Document.Create(ctx, opensearchapi.DocumentCreateReq{Index: "orders", DocumentID: "o1", Body: strings.NewReader(`{"total": 12.5, "customer": "alice", "placed": "2024-05-01T10:00:00Z"}`)})
	if err != nil || docRes.Result != "created" {
		t.Fatalf("create %v %v", docRes, err)
	}
	_, err = client.Document.Create(ctx, opensearchapi.DocumentCreateReq{Index: "orders", DocumentID: "o1", Body: strings.NewReader(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "version_conflict_engine_exception") {
		t.Fatalf("expected conflict, got %v", err)
	}
	bulk, err := client.Bulk(ctx, opensearchapi.BulkReq{Index: "orders", Body: strings.NewReader(`{"index": {"_id": "o2"}}
{"total": 40, "customer": "bob", "placed": "2024-05-02T10:00:00Z"}
{"index": {"_id": "o3"}}
{"total": 7, "customer": "alice", "placed": "2024-06-02T10:00:00Z"}
`)})
	if err != nil || bulk.Errors {
		t.Fatalf("bulk %v %v", bulk, err)
	}
	get, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "orders", DocumentID: "o2"})
	if err != nil || !get.Found || get.Version != 1 {
		t.Fatalf("get %v %v", get, err)
	}
	var src map[string]any
	_ = json.Unmarshal(get.Source, &src)
	if src["customer"] != "bob" {
		t.Fatalf("source %v", src)
	}
	upd, err := client.Update(ctx, opensearchapi.UpdateReq{Index: "orders", DocumentID: "o2", Body: strings.NewReader(`{"doc": {"total": 41}}`)})
	if err != nil || upd.Result != "updated" {
		t.Fatalf("update %v %v", upd, err)
	}
	// search with aggregations
	sres, err := client.Search(ctx, &opensearchapi.SearchReq{Indices: []string{"orders"}, Body: strings.NewReader(`{
	  "query": {"bool": {"filter": [{"range": {"placed": {"gte": "2024-05-01", "lt": "2024-06-01"}}}]}},
	  "sort": [{"total": "desc"}],
	  "aggs": {"by_customer": {"terms": {"field": "customer"}, "aggs": {"sum": {"sum": {"field": "total"}}}}}
	}`)})
	if err != nil {
		t.Fatal(err)
	}
	if sres.Hits.Total.Value != 2 || sres.Hits.Hits[0].ID != "o2" {
		t.Fatalf("search %+v", sres.Hits)
	}
	var aggs struct {
		ByCustomer struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int    `json:"doc_count"`
				Sum      struct {
					Value float64 `json:"value"`
				} `json:"sum"`
			} `json:"buckets"`
		} `json:"by_customer"`
	}
	if err := json.Unmarshal(sres.Aggregations, &aggs); err != nil {
		t.Fatal(err)
	}
	if len(aggs.ByCustomer.Buckets) != 2 || aggs.ByCustomer.Buckets[0].Key != "alice" && aggs.ByCustomer.Buckets[0].Key != "bob" {
		t.Fatalf("aggs %+v", aggs)
	}
	// search the seeded index through the same client
	sres, err = client.Search(ctx, &opensearchapi.SearchReq{Indices: []string{"products"}, Body: strings.NewReader(`{"query": {"match": {"name": "apple"}}}`)})
	if err != nil || sres.Hits.Total.Value != 2 {
		t.Fatalf("seed search %v %v", sres, err)
	}
	// count / delete / missing index error
	cnt, err := client.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{"orders"}})
	if err != nil || cnt.Count != 3 {
		t.Fatalf("count %v %v", cnt, err)
	}
	del, err := client.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{Index: "orders", DocumentID: "o3"})
	if err != nil || del.Result != "deleted" {
		t.Fatalf("delete %v %v", del, err)
	}
	_, err = client.Search(ctx, &opensearchapi.SearchReq{Indices: []string{"nope"}, Body: strings.NewReader(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "index_not_found_exception") {
		t.Fatalf("expected index_not_found, got %v", err)
	}
	// aliases and cat
	if _, err := client.Aliases(ctx, opensearchapi.AliasesReq{Body: strings.NewReader(`{"actions": [{"add": {"index": "orders", "alias": "current-orders"}}]}`)}); err != nil {
		t.Fatal(err)
	}
	cat, err := client.Cat.Indices(ctx, &opensearchapi.CatIndicesReq{})
	if err != nil || len(cat.Indices) != 2 {
		t.Fatalf("cat %v %v", cat, err)
	}
	// scroll through the client
	sres, err = client.Search(ctx, &opensearchapi.SearchReq{Indices: []string{"products"}, Params: opensearchapi.SearchParams{Scroll: 60 * 1e9, Size: intPtr(2)}, Body: strings.NewReader(`{"sort": ["_doc"]}`)})
	if err != nil || len(sres.Hits.Hits) != 2 || sres.ScrollID == nil {
		t.Fatalf("scroll start %v %v", sres, err)
	}
	total := len(sres.Hits.Hits)
	for {
		scr, err := client.Scroll.Get(ctx, opensearchapi.ScrollGetReq{ScrollID: *sres.ScrollID, Params: opensearchapi.ScrollGetParams{Scroll: 60 * 1e9}})
		if err != nil {
			t.Fatal(err)
		}
		if len(scr.Hits.Hits) == 0 {
			break
		}
		total += len(scr.Hits.Hits)
	}
	if total != 5 {
		t.Fatalf("scroll total %d", total)
	}
	if _, err := client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{"orders"}}); err != nil {
		t.Fatal(err)
	}
	// the base cluster is untouched
	if n, _ := base.Count("products", nil); n != 5 {
		t.Fatalf("base count %d", n)
	}
	if names := base.Indices(); len(names) != 1 {
		t.Fatalf("base indices %v", names)
	}
}

func intPtr(n int) *int { return &n }
