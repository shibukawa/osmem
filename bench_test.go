package osmem

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const benchMapping = `{"mappings": {"properties": {"title": {"type": "text", "fields": {"keyword": {"type": "keyword"}}}, "category": {"type": "keyword"}, "price": {"type": "double"}, "created": {"type": "date"}}}}`

// benchDoc returns the source of the i-th benchmark document.
func benchDoc(i int) string {
	return fmt.Sprintf(`{"title":"item number %d in category %d","category":"c%d","price":%d.5,"created":"2024-01-%02dT00:00:00Z"}`, i, i%10, i%10, i%1000, i%28+1)
}

func benchCluster(b *testing.B, n int) *Cluster {
	c := New()
	if err := c.CreateIndex("items", benchMapping); err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":\"items\",\"_id\":\"%d\"}}\n%s\n", i, benchDoc(i))
		if (i+1)%10000 == 0 || i == n-1 {
			if err := c.BulkString(sb.String()); err != nil {
				b.Fatal(err)
			}
			sb.Reset()
		}
	}
	return c
}

// liveHeap returns the heap in use after a collection.
func liveHeap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func BenchmarkBulkIndex10k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		c := benchCluster(b, 10000)
		c.Close()
	}
}

// BenchmarkSingleDocWrites indexes documents with one request each, so every
// write commits a bleve batch of its own, and reports the heap the cluster
// retains afterwards, the time per write and the latency of a term query.
func BenchmarkSingleDocWrites(b *testing.B) {
	const query = `{"query": {"term": {"title.keyword": "item number 5 in category 5"}}}`
	for _, n := range []int{1000, 5000} {
		b.Run("docs="+strconv.Itoa(n), func(b *testing.B) {
			var heapMiB, writeNs, queryNs float64
			for i := 0; i < b.N; i++ {
				before := liveHeap()
				c := New()
				if err := c.CreateIndex("items", benchMapping); err != nil {
					b.Fatal(err)
				}
				start := time.Now()
				for j := 0; j < n; j++ {
					if err := c.Index("items", strconv.Itoa(j), benchDoc(j)); err != nil {
						b.Fatal(err)
					}
				}
				writeNs = float64(time.Since(start).Nanoseconds()) / float64(n)
				heapMiB = float64(liveHeap()-before) / (1 << 20)
				const queries = 100
				start = time.Now()
				for q := 0; q < queries; q++ {
					res, err := c.Do(http.MethodPost, "/items/_search", query)
					if err != nil || res.IsError() {
						b.Fatal(err, string(res.Body))
					}
				}
				queryNs = float64(time.Since(start).Nanoseconds()) / queries
				c.Close()
			}
			b.ReportMetric(heapMiB, "heap-MiB")
			b.ReportMetric(writeNs, "ns/write")
			b.ReportMetric(queryNs, "ns/term-query")
		})
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

// BenchmarkSort100k sorts every document of a 100,000-document index by one
// or two fields and returns the first page, so the time goes to sorting
// rather than matching.
func BenchmarkSort100k(b *testing.B) {
	c := benchCluster(b, 100000)
	defer c.Close()
	for _, bc := range []struct{ name, sort string }{
		{"double", `[{"price": "desc"}]`},
		{"date", `[{"created": "asc"}]`},
		{"keyword", `[{"title.keyword": "asc"}]`},
		{"two_keys", `[{"category": "asc"}, {"price": "desc"}]`},
	} {
		body := `{"query": {"match_all": {}}, "size": 10, "sort": ` + bc.sort + `}`
		b.Run(bc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				res, err := c.Do(http.MethodPost, "/items/_search", body)
				if err != nil || res.IsError() {
					b.Fatal(err, string(res.Body))
				}
			}
		})
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
