package osmemtest_test

import (
	"net/http"
	"testing"

	"github.com/shibukawa/osmem"
	"github.com/shibukawa/osmem/osmemtest"
)

func TestCloneAndServe(t *testing.T) {
	base := osmem.New()
	defer base.Close()
	if err := base.Index("items", "1", map[string]any{"name": "base"}); err != nil {
		t.Fatal(err)
	}
	var clone *osmem.Cluster
	var srv *osmem.Server
	t.Run("inner", func(t *testing.T) {
		clone, srv = osmemtest.CloneAndServe(t, base)
		if err := clone.Index("items", "2", map[string]any{"name": "clone"}); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(srv.URL + "/items/_count")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	// after the subtest ended, the server is closed and the base untouched
	if _, err := http.Get(srv.URL + "/items/_count"); err == nil {
		t.Fatal("server should be closed after cleanup")
	}
	if n, _ := base.Count("items", nil); n != 1 {
		t.Fatalf("base count %d", n)
	}
	c := osmemtest.New(t)
	if err := c.CreateIndex("x", nil); err != nil {
		t.Fatal(err)
	}
}
