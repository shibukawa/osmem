package osmem

import (
	"net/http"
	"strings"
	"testing"
)

// from and size are Java ints: a 64-bit value must fail to parse instead
// of overflowing from+size past the max_result_window check.
func TestSearchFromSizeAreInts(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	res, err := c.Do(http.MethodGet, "/products/_search?from=9223372036854775807&size=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(res.Body), "Failed to parse int parameter [from]") {
		t.Fatalf("status %d %s", res.StatusCode, res.Body)
	}
	res, err = c.Do(http.MethodGet, "/products/_search?from=10000&size=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(res.Body), "Result window is too large") {
		t.Fatalf("status %d %s", res.StatusCode, res.Body)
	}
	res, err = c.Do(http.MethodGet, "/products/_search?from=1&size=2", nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("plain paging: %v %d %s", err, res.StatusCode, res.Body)
	}
}
