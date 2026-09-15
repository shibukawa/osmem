package osmem

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// These regression probes assert behavior observed on OpenSearch 3.8.0.
func TestCompatibilityProbes(t *testing.T) {
	type request struct {
		Method, Path, Body string
		Status             int
		Checks             map[string]any
	}
	var cases []struct {
		ID, Source string
		Setup      []request
		Request    request
		After      []request
	}
	f, err := os.Open("testdata/compatibility/probes.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.UseNumber()
	if err := d.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	var observations []map[string]any
	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			c := New()
			defer c.Close()
			run := func(req request) (int, any) {
				r := httptest.NewRequest(req.Method, req.Path, strings.NewReader(req.Body))
				r.Header.Set("Content-Type", "application/json")
				if req.Path == "/_bulk" {
					r.Header.Set("Content-Type", "application/x-ndjson")
				}
				w := httptest.NewRecorder()
				c.Handler().ServeHTTP(w, r)
				var body any
				d := json.NewDecoder(w.Body)
				d.UseNumber()
				if err := d.Decode(&body); err != nil {
					t.Fatalf("decode %s: %v", req.Path, err)
				}
				return w.Code, body
			}
			for _, req := range tc.Setup {
				code, body := run(req)
				if code != req.Status {
					t.Fatalf("setup %s: status %d, body %v", req.Path, code, body)
				}
			}
			code, body := run(tc.Request)
			var differences []string
			if code != tc.Request.Status {
				differences = append(differences, fmt.Sprintf("status: want %d, got %d", tc.Request.Status, code))
			}
			for pointer, want := range tc.Request.Checks {
				got, exists := compatibilityPointer(body, pointer)
				if !exists || !reflect.DeepEqual(got, want) {
					differences = append(differences, fmt.Sprintf("%s: want %v, got %v (exists=%t)", pointer, want, got, exists))
				}
			}
			var after []map[string]any
			for _, req := range tc.After {
				status, result := run(req)
				after = append(after, map[string]any{"path": req.Path, "status": status, "body": result})
				if status != req.Status {
					differences = append(differences, fmt.Sprintf("after %s: want %d, got %d", req.Path, req.Status, status))
				}
			}
			observations = append(observations, map[string]any{"id": tc.ID, "source": tc.Source, "status": code, "body": body, "differences": differences, "after": after})
			for _, diff := range differences {
				t.Error(diff)
			}
		})
	}
	if path := os.Getenv("OSMEM_COMPAT_REPORT"); path != "" {
		data, err := json.MarshalIndent(observations, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func compatibilityPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			value = v[i]
		default:
			return nil, false
		}
	}
	return value, true
}
