package engine

import (
	"sort"
	"strings"
)

// Template is an index template (composable or legacy).
type Template struct {
	Name     string
	Patterns []string
	Priority int
	Settings M // normalized {"index": {...}}
	Mappings M
	Aliases  M
	Raw      M
	Version  any
}

func (t *Template) matches(name string) bool {
	for _, p := range t.Patterns {
		if wildcardMatch(p, name) {
			return true
		}
	}
	return false
}

func parseTemplate(name string, body M, legacy bool) (*Template, error) {
	t := &Template{Name: name, Raw: body, Settings: M{}, Mappings: M{}, Aliases: M{}}
	t.Patterns = getStrings(body, "index_patterns")
	if len(t.Patterns) == 0 {
		if legacy {
			t.Patterns = getStrings(body, "template")
		}
		if len(t.Patterns) == 0 {
			return nil, errIllegalArgument("index patterns are missing")
		}
	}
	t.Version = body["version"]
	var src M
	if legacy {
		t.Priority = getInt(body, "order", 0)
		src = body
	} else {
		t.Priority = getInt(body, "priority", 0)
		src = getMap(body, "template")
	}
	if s, ok := src["settings"].(M); ok {
		t.Settings = normalizeSettings(s)
	}
	if m, ok := src["mappings"].(M); ok {
		if doc, ok := m["_doc"].(M); ok && len(m) == 1 {
			m = doc
		}
		if _, err := parseMapping(m); err != nil {
			return nil, err
		}
		t.Mappings = m
	}
	if a, ok := src["aliases"].(M); ok {
		t.Aliases = a
	}
	if !legacy {
		// report the template the way OpenSearch does: nested settings,
		// explicit priority and composed_of
		raw := cloneDeep(body).(M)
		raw["index_patterns"] = t.Patterns
		if _, ok := raw["composed_of"]; !ok {
			raw["composed_of"] = []any{}
		}
		if _, ok := raw["priority"]; !ok {
			raw["priority"] = 0
		}
		tpl, _ := raw["template"].(M)
		if tpl != nil {
			if _, ok := tpl["settings"]; ok {
				tpl["settings"] = t.Settings
			}
		}
		t.Raw = raw
	}
	return t, nil
}

// PutIndexTemplate implements PUT /_index_template/{name}.
func (c *Cluster) PutIndexTemplate(name string, body M) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := parseTemplate(name, body, false)
	if err != nil {
		return fail(err)
	}
	c.templates[name] = t
	return ok(M{"acknowledged": true})
}

// GetIndexTemplate implements GET /_index_template/{name}.
func (c *Cluster) GetIndexTemplate(name string) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var list []any
	names := make([]string, 0, len(c.templates))
	for n := range c.templates {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if name != "" && name != "*" && !matchAny(splitList(name), n) {
			continue
		}
		list = append(list, M{"name": n, "index_template": c.templates[n].Raw})
	}
	if list == nil {
		if name != "" && !strings.ContainsAny(name, "*?") {
			return fail(&Error{Status: 404, Type: "resource_not_found_exception", Reason: "index template matching [" + name + "] not found"})
		}
		list = []any{}
	}
	return ok(M{"index_templates": list})
}

// IndexTemplateExists implements HEAD /_index_template/{name}.
func (c *Cluster) IndexTemplateExists(name string) (Response, error) {
	r, err := c.GetIndexTemplate(name)
	if err != nil || r.Status != 200 {
		return Response{Status: 404}, nil
	}
	if list, ok := r.Body.(M)["index_templates"].([]any); ok && len(list) == 0 {
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

// DeleteIndexTemplate implements DELETE /_index_template/{name}.
func (c *Cluster) DeleteIndexTemplate(name string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for n := range c.templates {
		if matchAny(splitList(name), n) {
			delete(c.templates, n)
			found = true
		}
	}
	if !found {
		return fail(&Error{Status: 404, Type: "resource_not_found_exception", Reason: "index_template [" + name + "] missing"})
	}
	return ok(M{"acknowledged": true})
}

// PutLegacyTemplate implements PUT /_template/{name}.
func (c *Cluster) PutLegacyTemplate(name string, body M) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := parseTemplate(name, body, true)
	if err != nil {
		return fail(err)
	}
	c.legacyTemplates[name] = t
	return ok(M{"acknowledged": true})
}

// GetLegacyTemplate implements GET /_template/{name}.
func (c *Cluster) GetLegacyTemplate(name string) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := M{}
	for n, t := range c.legacyTemplates {
		if name != "" && !matchAny(splitList(name), n) {
			continue
		}
		body := M{"order": t.Priority, "index_patterns": t.Patterns, "settings": t.Settings, "mappings": t.Mappings, "aliases": t.Aliases}
		if t.Version != nil {
			body["version"] = t.Version
		}
		out[n] = body
	}
	if len(out) == 0 && name != "" && !strings.ContainsAny(name, "*?") {
		return Response{Status: 404, Body: M{}}, nil
	}
	return ok(out)
}

// LegacyTemplateExists implements HEAD /_template/{name}.
func (c *Cluster) LegacyTemplateExists(name string) (Response, error) {
	r, _ := c.GetLegacyTemplate(name)
	if r.Status != 200 {
		return Response{Status: 404}, nil
	}
	if m, ok := r.Body.(M); ok && len(m) == 0 {
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

// DeleteLegacyTemplate implements DELETE /_template/{name}.
func (c *Cluster) DeleteLegacyTemplate(name string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for n := range c.legacyTemplates {
		if matchAny(splitList(name), n) {
			delete(c.legacyTemplates, n)
			found = true
		}
	}
	if !found {
		return fail(&Error{Status: 404, Type: "index_template_missing_exception", Reason: "index_template [" + name + "] missing"})
	}
	return ok(M{"acknowledged": true})
}
