package osmem

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadSeed loads fixture data from a path.
//
// A directory is read in this order (files sorted by name within a step):
//
//	<name>.template.json   PUT /_index_template/<name> body
//	<index>.index.json     PUT /<index> body (settings, mappings, aliases)
//	<index>.ndjson         POST /<index>/_bulk body (actions may override _index)
//	aliases.json           POST /_aliases body
//
// A single file ending in .ndjson is loaded as a _bulk body (indices are
// auto-created with dynamic mapping unless they exist). Any failed bulk
// item makes LoadSeed return an error.
func (c *Cluster) LoadSeed(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if !strings.HasSuffix(path, ".ndjson") {
			return fmt.Errorf("osmem: seed file %s: only .ndjson files or directories are supported", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := c.Bulk(f); err != nil {
			return fmt.Errorf("osmem: seed %s: %w", path, err)
		}
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var templates, indices, bulks []string
	aliases := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".template.json"):
			templates = append(templates, name)
		case strings.HasSuffix(name, ".index.json"):
			indices = append(indices, name)
		case strings.HasSuffix(name, ".ndjson"):
			bulks = append(bulks, name)
		case name == "aliases.json":
			aliases = name
		}
	}
	sort.Strings(templates)
	sort.Strings(indices)
	sort.Strings(bulks)
	do := func(method, url, file string) error {
		data, err := os.ReadFile(filepath.Join(path, file))
		if err != nil {
			return err
		}
		res, err := c.Do(method, url, data)
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return fmt.Errorf("osmem: seed %s: %w", file, err)
		}
		return nil
	}
	for _, f := range templates {
		if err := do(http.MethodPut, "/_index_template/"+strings.TrimSuffix(f, ".template.json"), f); err != nil {
			return err
		}
	}
	for _, f := range indices {
		if err := do(http.MethodPut, "/"+strings.TrimSuffix(f, ".index.json"), f); err != nil {
			return err
		}
	}
	for _, f := range bulks {
		file, err := os.Open(filepath.Join(path, f))
		if err != nil {
			return err
		}
		index := strings.TrimSuffix(f, ".ndjson")
		res, err := c.Do(http.MethodPost, "/"+index+"/_bulk", file)
		file.Close()
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return fmt.Errorf("osmem: seed %s: %w", f, err)
		}
		if err := bulkResponseError(res); err != nil {
			return fmt.Errorf("osmem: seed %s: %w", f, err)
		}
	}
	if aliases != "" {
		if err := do(http.MethodPost, "/_aliases", aliases); err != nil {
			return err
		}
	}
	return nil
}
