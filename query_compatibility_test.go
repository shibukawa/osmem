package osmem

import (
	"net/http"
	"testing"
)

func TestBoolMinimumShouldMatchZeroCompatibility(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()

	t.Run("should clauses are optional without required clauses", func(t *testing.T) {
		ids, _ := search(t, c, `{"query":{"bool":{"should":[{"term":{"tags":"red"}}],"minimum_should_match":0}},"sort":["_doc"]}`)
		assertSet(t, ids, "1", "2", "3", "4", "5")
	})

	t.Run("must clause still applies", func(t *testing.T) {
		ids, _ := search(t, c, `{"query":{"bool":{"must":[{"term":{"tags":"fruit"}}],"should":[{"term":{"tags":"vegetable"}}],"minimum_should_match":0}},"sort":["_doc"]}`)
		assertSet(t, ids, "1", "2", "3", "5")
	})

	// Confirm an explicit zero remains a valid query argument at the HTTP API.
	statusCode, _ := status(t, c, http.MethodPost, "/products/_search", `{"query":{"bool":{"should":[{"term":{"tags":"absent"}}],"minimum_should_match":0}}}`)
	if statusCode != http.StatusOK {
		t.Fatalf("minimum_should_match:0 status = %d, want %d", statusCode, http.StatusOK)
	}
}
