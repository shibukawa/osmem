package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunWithSeedAndStdinExit(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	var stdout bytes.Buffer
	readyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(context.Background(), Options{
			Seeds: []string{"testdata/seed"}, Japanese: true, StdinWatch: true, Stdin: stdinR,
			Stdout: &stdout, Stderr: io.Discard, Ready: func(url string) { readyCh <- url },
		})
	}()
	var url string
	select {
	case url = <-readyCh:
	case err := <-errCh:
		t.Fatalf("run failed: %v", err)
	case <-time.After(120 * time.Second):
		t.Fatal("server did not become ready")
	}
	var ready Ready
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &ready); err != nil || ready.URL != url || ready.PID == 0 {
		t.Fatalf("ready line %q: %v", stdout.String(), err)
	}
	if strings.Join(ready.Indices, ",") != "other,products" {
		t.Fatalf("indices %v", ready.Indices)
	}
	get := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, url+path, strings.NewReader(body))
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
	st, cnt := get(http.MethodGet, "/items/_count", "")
	if st != 200 || cnt["count"].(float64) != 2 {
		t.Fatalf("seed via alias: %d %v", st, cnt)
	}
	st, res := get(http.MethodPost, "/products/_search", `{"query": {"match": {"name": "スカイツリー"}}}`)
	if st != 200 || res["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("kuromoji search: %d %v", st, res)
	}
	st, _ = get(http.MethodPut, "/logs-1/_doc/1", `{"ts": "2024-01-01"}`)
	if st != 201 {
		t.Fatalf("template applied: %d", st)
	}
	st, m := get(http.MethodGet, "/logs-1/_mapping", "")
	if m["logs-1"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)["ts"].(map[string]any)["type"] != "date" {
		t.Fatalf("template mapping %v", m)
	}
	st, clone := get(http.MethodPost, "/_osmem/clones", "")
	if st != 201 {
		t.Fatalf("clone %d %v", st, clone)
	}
	// closing stdin stops the server
	stdinW.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit on stdin EOF")
	}
	if _, err := http.Get(url + "/"); err == nil {
		t.Fatal("server still listening")
	}
}

func TestRunSeedError(t *testing.T) {
	err := Run(context.Background(), Options{Seeds: []string{"testdata/missing"}, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil {
		t.Fatal("expected error for missing seed")
	}
}
