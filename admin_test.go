package osmem

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestManagementAPI(t *testing.T) {
	base := seedCluster(t)
	defer base.Close()
	srv := base.MustServe()
	defer srv.Close()
	get := func(method, url string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	st, info := get(http.MethodGet, srv.URL+"/_osmem")
	if st != 200 || info["frozen"] != false {
		t.Fatalf("info %d %v", st, info)
	}
	// writes are allowed before freezing
	st, _ = get(http.MethodPut, srv.URL+"/products/_doc/50")
	if st != 201 {
		t.Fatalf("write before freeze %d", st)
	}
	st, clone := get(http.MethodPost, srv.URL+"/_osmem/clones")
	if st != 201 || clone["id"] == "" || !strings.HasPrefix(clone["url"].(string), "http://127.0.0.1:") {
		t.Fatalf("create clone %d %v", st, clone)
	}
	// base is frozen now
	st, body := get(http.MethodPut, srv.URL+"/products/_doc/51")
	if st != 403 || errType(body) != "osmem_base_frozen" {
		t.Fatalf("frozen write %d %v", st, body)
	}
	st, _ = get(http.MethodPost, srv.URL+"/products/_search")
	if st != 200 {
		t.Fatalf("frozen search %d", st)
	}
	st, _ = get(http.MethodPost, srv.URL+"/products/_refresh")
	if st != 200 {
		t.Fatalf("frozen refresh %d", st)
	}
	st, _ = get(http.MethodPut, srv.URL+"/products/_mapping")
	if st != 403 {
		t.Fatalf("frozen mapping %d", st)
	}
	// the clone accepts writes and is isolated
	cloneURL := clone["url"].(string)
	st, _ = get(http.MethodPut, cloneURL+"/products/_doc/51")
	if st != 201 {
		t.Fatalf("clone write %d", st)
	}
	st, cnt := get(http.MethodGet, cloneURL+"/products/_count")
	if st != 200 || cnt["count"].(float64) != 7 {
		t.Fatalf("clone count %v", cnt)
	}
	st, cnt = get(http.MethodGet, srv.URL+"/products/_count")
	if cnt["count"].(float64) != 6 {
		t.Fatalf("base count %v", cnt)
	}
	st, list := get(http.MethodGet, srv.URL+"/_osmem/clones")
	if st != 200 || len(list["clones"].([]any)) != 1 {
		t.Fatalf("list %v", list)
	}
	st, _ = get(http.MethodDelete, srv.URL+"/_osmem/clones/"+clone["id"].(string))
	if st != 200 {
		t.Fatalf("delete clone %d", st)
	}
	if _, err := http.Get(cloneURL + "/"); err == nil {
		t.Fatal("clone server should be closed")
	}
	st, _ = get(http.MethodDelete, srv.URL+"/_osmem/clones/nope")
	if st != 404 {
		t.Fatalf("delete missing %d", st)
	}
	st, _ = get(http.MethodPost, srv.URL+"/_osmem/base/unfreeze")
	st, _ = get(http.MethodPut, srv.URL+"/products/_doc/52")
	if st != 201 {
		t.Fatalf("write after unfreeze %d", st)
	}
	// closing the base closes remaining managed clones
	_, c2 := get(http.MethodPost, srv.URL+"/_osmem/clones")
	base.Close()
	if _, err := http.Get(c2["url"].(string) + "/"); err == nil {
		t.Fatal("managed clone should be closed with the base")
	}
}
