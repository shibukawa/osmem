package osmem

import "testing"

// more_like_this, checked against the OpenSearch REST API test suite
// (test/mlt/10_basic.yml, 20_docs.yml, 30_unlike.yml).
func TestMoreLikeThisCompatibility(t *testing.T) {
	c := New()
	defer c.Close()

	t.Run("basic: single doc excluded by default min_term_freq/min_doc_freq", func(t *testing.T) {
		dslIndex(t, c, "mlt1", `{"mappings":{"properties":{"foo":{"type":"text"},"title":{"type":"text"}}}}`,
			`{"foo":"bar","title":"howdy"}`)
		ids, _, st, res := dslSearch(t, c, "mlt1", `{"more_like_this":{"like":[{"_id":"1"}],"fields":["title"]}}`)
		if st != 200 || len(ids) != 0 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
	})

	t.Run("like docs and stored ids, include and zeroed frequency filters", func(t *testing.T) {
		dslIndex(t, c, "mlt2", `{}`, `{"foo":"bar"}`, `{"foo":"baz"}`, `{"foo":"foo"}`)
		ids, _, st, res := dslSearch(t, c, "mlt2", `{"more_like_this":{"like":[{"_index":"mlt2","doc":{"foo":"bar"}},{"_index":"mlt2","_id":"2"},{"_id":"3"}],"include":true,"min_doc_freq":0,"min_term_freq":0}}`)
		if st != 200 || len(ids) != 3 {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
	})

	t.Run("unlike removes shared significant terms", func(t *testing.T) {
		dslIndex(t, c, "mlt3", `{}`, `{"foo":"bar baz selected"}`, `{"foo":"bar"}`, `{"foo":"bar baz"}`)
		ids, _, st, res := dslSearch(t, c, "mlt3", `{"more_like_this":{"like":{"_index":"mlt3","_id":"1"},"unlike":{"_index":"mlt3","_id":"3"},"include":true,"min_doc_freq":0,"min_term_freq":0}}`)
		if st != 200 || len(ids) != 1 || ids[0] != "1" {
			t.Fatalf("status=%d ids=%v body=%v", st, ids, res)
		}
	})
}
