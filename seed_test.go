package osmem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSeed(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.index.json", `{"mappings": {"properties": {"n": {"type": "integer"}}}}`)
	write("a.ndjson", "{\"index\": {\"_id\": \"1\"}}\n{\"n\": 1}\n{\"index\": {\"_id\": \"2\"}}\n{\"n\": 2}\n")
	write("aliases.json", `{"actions": [{"add": {"index": "a", "alias": "alias-a"}}]}`)
	c := New()
	defer c.Close()
	if err := c.LoadSeed(dir); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Count("alias-a", nil); n != 2 {
		t.Fatalf("count %d", n)
	}
	write("b.ndjson", "{\"index\": {\"_id\": \"1\"}}\n{\"n\": \"bad\"}\n")
	write("b.index.json", `{"mappings": {"properties": {"n": {"type": "integer"}}}}`)
	c2 := New()
	defer c2.Close()
	if err := c2.LoadSeed(dir); err == nil {
		t.Fatal("expected bulk failure")
	}
	single := filepath.Join(dir, "single.ndjson")
	_ = os.WriteFile(single, []byte("{\"index\": {\"_index\": \"s\", \"_id\": \"1\"}}\n{\"x\": 1}\n"), 0o644)
	c3 := New()
	defer c3.Close()
	if err := c3.LoadSeed(single); err != nil {
		t.Fatal(err)
	}
	if n, _ := c3.Count("s", nil); n != 1 {
		t.Fatalf("single file count %d", n)
	}
}
