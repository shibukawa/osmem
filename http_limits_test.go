package osmem

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body larger than maxBodyBytes is refused with 413 before it is read
// into memory, whether it arrives plain or gzip-compressed.
func TestRequestBodyLimit(t *testing.T) {
	c := New()
	defer c.Close()
	huge := make([]byte, maxBodyBytes+1)
	for i := range huge {
		huge[i] = ' '
	}
	req := httptest.NewRequest(http.MethodPost, "/products/_search", bytes.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("plain: status %d %s", rec.Code, rec.Body.String())
	}

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(huge)
	_ = w.Close()
	req = httptest.NewRequest(http.MethodPost, "/products/_search", &gz)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec = httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip: status %d %s", rec.Code, rec.Body.String())
	}
}

// A frozen base refuses PUT /_cluster/settings like every other write.
func TestFrozenRejectsClusterSettings(t *testing.T) {
	c := New()
	defer c.Close()
	c.Freeze()
	res, err := c.Do(http.MethodPut, "/_cluster/settings", `{"persistent": {"cluster.max_shards_per_node": 2000}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d %s", res.StatusCode, res.Body)
	}
	res, err = c.Do(http.MethodGet, "/_cluster/settings", nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("read on frozen base: %v %d", err, res.StatusCode)
	}
}

// Helper paths are escaped per segment: an id with a space or a slash
// round-trips instead of panicking or being cut at the slash.
func TestHelperPathEscaping(t *testing.T) {
	c := New()
	defer c.Close()
	if err := c.Index("products", "my id/1", map[string]any{"name": "x"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	found, err := c.Get("products", "my id/1", &doc)
	if err != nil || !found || doc["name"] != "x" {
		t.Fatalf("get: %v %v %v", err, found, doc)
	}
	if _, err := c.Do(http.MethodGet, "/products/_doc/my id", nil); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("Do with whitespace: %v", err)
	}
}
