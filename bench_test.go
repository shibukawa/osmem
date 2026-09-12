package osmem

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func benchCluster(b *testing.B, n int) *Cluster {
	c := New()
	if err := c.CreateIndex("items", `{"mappings": {"properties": {"title": {"type": "text", "fields": {"keyword": {"type": "keyword"}}}, "category": {"type": "keyword"}, "price": {"type": "double"}, "created": {"type": "date"}}}}`); err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":\"items\",\"_id\":\"%d\"}}\n{\"title\":\"item number %d in category %d\",\"category\":\"c%d\",\"price\":%d.5,\"created\":\"2024-01-%02dT00:00:00Z\"}\n", i, i, i%10, i%10, i%1000, i%28+1)
	}
	if err := c.BulkString(sb.String()); err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkBulkIndex10k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		c := benchCluster(b, 10000)
		c.Close()
	}
}

func BenchmarkCloneReadOnly(b *testing.B) {
	base := benchCluster(b, 10000)
	defer base.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := base.Clone()
		if _, err := c.Count("items", `{"query": {"term": {"category": "c3"}}}`); err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

func BenchmarkCloneFirstWrite10k(b *testing.B) {
	base := benchCluster(b, 10000)
	defer base.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := base.Clone()
		if err := c.Index("items", "new", map[string]any{"title": "new item", "category": "c1", "price": 1.0}); err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

func BenchmarkSearch10k(b *testing.B) {
	c := benchCluster(b, 10000)
	defer c.Close()
	body := `{"query": {"bool": {"must": [{"match": {"title": "item"}}], "filter": [{"term": {"category": "c3"}}, {"range": {"price": {"gte": 100}}}]}}, "sort": [{"price": "desc"}], "size": 10, "aggs": {"by_day": {"date_histogram": {"field": "created", "calendar_interval": "day"}}}}`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.Do(http.MethodPost, "/items/_search", body)
		if err != nil || res.IsError() {
			b.Fatal(err, string(res.Body))
		}
	}
}

func BenchmarkTermQuery10k(b *testing.B) {
	c := benchCluster(b, 10000)
	defer c.Close()
	body := `{"query": {"term": {"title.keyword": "item number 5 in category 5"}}}`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.Do(http.MethodPost, "/items/_search", body)
		if err != nil || res.IsError() {
			b.Fatal(err, string(res.Body))
		}
	}
}
