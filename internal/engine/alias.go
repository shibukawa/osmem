package engine

import (
	"sort"
	"strings"
)

func parseAlias(spec M) (*Alias, error) {
	a := &Alias{}
	if spec == nil {
		return a, nil
	}
	if f, ok := spec["filter"]; ok {
		fm, ok := f.(M)
		if !ok {
			return nil, errParsing("alias filter must be an object")
		}
		a.Filter = fm
	}
	if v, ok := spec["is_write_index"]; ok {
		b := getBool(M{"v": v}, "v", false)
		a.IsWriteIndex = &b
	}
	a.Routing = getString(spec, "routing")
	a.IndexRouting = getString(spec, "index_routing")
	a.SearchRouting = getString(spec, "search_routing")
	return a, nil
}

// UpdateAliases implements POST /_aliases.
func (c *Cluster) UpdateAliases(body M) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	actions, isList := body["actions"].([]any)
	if !isList {
		return fail(errParsing("[aliases] Unexpected field [actions]"))
	}
	type op struct {
		kind    string
		indices []*Index
		aliases []string
		alias   *Alias
	}
	var ops []op
	for _, raw := range actions {
		am, ok := raw.(M)
		if !ok || len(am) != 1 {
			return fail(errParsing("[aliases] each action must have exactly one operation"))
		}
		for kind, specRaw := range am {
			spec, _ := specRaw.(M)
			var names []string
			names = append(names, getStrings(spec, "index")...)
			names = append(names, getStrings(spec, "indices")...)
			var aliases []string
			aliases = append(aliases, getStrings(spec, "alias")...)
			aliases = append(aliases, getStrings(spec, "aliases")...)
			if len(names) == 0 {
				return fail(errActionRequestValidation("Alias action [" + kind + "]: [index/indices] may not be empty"))
			}
			var indices []*Index
			for _, n := range names {
				ts, err := c.resolve(n, resolveOptions{allowAliases: false, allowNoIndices: true})
				if err != nil {
					return fail(err)
				}
				for _, t := range ts {
					indices = append(indices, t.ix)
				}
			}
			switch kind {
			case "add":
				if len(aliases) == 0 {
					return fail(errActionRequestValidation("Alias action [add]: [alias/aliases] may not be empty"))
				}
				a, err := parseAlias(spec)
				if err != nil {
					return fail(err)
				}
				for _, al := range aliases {
					if _, ok := c.indices[al]; ok {
						return fail(&Error{Status: 400, Type: "invalid_alias_name_exception", Reason: "Invalid alias name [" + al + "]: an index or data stream exists with the same name as the alias"})
					}
				}
				ops = append(ops, op{kind: kind, indices: indices, aliases: aliases, alias: a})
			case "remove":
				if len(aliases) == 0 {
					return fail(errActionRequestValidation("Alias action [remove]: [alias/aliases] may not be empty"))
				}
				found := false
				for _, ix := range indices {
					for _, al := range aliases {
						for existing := range ix.Aliases {
							if wildcardMatch(al, existing) {
								found = true
							}
						}
					}
				}
				if !found {
					return fail(errAliasMissing(strings.Join(aliases, ",")))
				}
				ops = append(ops, op{kind: kind, indices: indices, aliases: aliases})
			case "remove_index":
				ops = append(ops, op{kind: kind, indices: indices})
			default:
				return fail(errParsing("[aliases] unknown action [%s]", kind))
			}
		}
	}
	for _, o := range ops {
		for _, target := range o.indices {
			if _, ok := c.indices[target.Name]; !ok {
				continue
			}
			ix, err := c.writable(target.Name)
			if err != nil {
				return fail(err)
			}
			switch o.kind {
			case "add":
				for _, al := range o.aliases {
					a := *o.alias
					ix.Aliases[al] = &a
				}
			case "remove":
				for _, al := range o.aliases {
					for existing := range ix.Aliases {
						if wildcardMatch(al, existing) {
							delete(ix.Aliases, existing)
						}
					}
				}
			case "remove_index":
				ix.release()
				delete(c.indices, ix.Name)
			}
		}
	}
	return ok(M{"acknowledged": true})
}

// PutAlias implements PUT /{index}/_alias/{name}.
func (c *Cluster) PutAlias(expr, name string, body M) (Response, error) {
	return c.UpdateAliases(M{"actions": []any{M{"add": mergeAliasSpec(body, M{"index": expr, "alias": name})}}})
}

func mergeAliasSpec(body M, extra M) M {
	out := M{}
	for k, v := range body {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// DeleteAlias implements DELETE /{index}/_alias/{name}.
func (c *Cluster) DeleteAlias(expr, name string) (Response, error) {
	return c.UpdateAliases(M{"actions": []any{M{"remove": M{"index": expr, "alias": name}}}})
}

// GetAliases implements GET /_alias, GET /{index}/_alias/{name}.
func (c *Cluster) GetAliases(expr, name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	patterns := splitList(name)
	out := M{}
	matched := false
	for _, t := range ts {
		aliases := M{}
		for an, a := range t.ix.Aliases {
			if len(patterns) > 0 && !matchAny(patterns, an) {
				continue
			}
			aliases[an] = a.toJSON()
			matched = true
		}
		if len(aliases) > 0 || len(patterns) == 0 {
			out[t.ix.Name] = M{"aliases": aliases}
		}
	}
	if len(patterns) > 0 && !matched {
		hasWildcard := false
		for _, p := range patterns {
			if strings.ContainsAny(p, "*?") {
				hasWildcard = true
			}
		}
		if !hasWildcard {
			return Response{Status: 404, Body: M{"error": "alias [" + name + "] missing", "status": 404}}, nil
		}
	}
	return ok(out)
}

// AliasExists implements HEAD /_alias/{name}.
func (c *Cluster) AliasExists(expr, name string, p Params) (Response, error) {
	r, err := c.GetAliases(expr, name, p)
	if err != nil || r.Status != 200 {
		return Response{Status: 404}, nil
	}
	if m, ok := r.Body.(M); ok && len(m) == 0 {
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == "_all" || wildcardMatch(p, s) {
			return true
		}
	}
	return false
}

// CatAliases lists aliases for _cat/aliases.
func (c *Cluster) CatAliases(name string) []M {
	c.mu.RLock()
	defer c.mu.RUnlock()
	patterns := splitList(name)
	var rows []M
	for _, ixName := range c.sortedIndexNames() {
		ix := c.indices[ixName]
		for an, a := range ix.Aliases {
			if len(patterns) > 0 && !matchAny(patterns, an) {
				continue
			}
			filter := "-"
			if a.Filter != nil {
				filter = "*"
			}
			wi := "-"
			if a.IsWriteIndex != nil {
				if *a.IsWriteIndex {
					wi = "true"
				} else {
					wi = "false"
				}
			}
			rows = append(rows, M{"alias": an, "index": ixName, "filter": filter, "routing.index": "-", "routing.search": "-", "is_write_index": wi})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["alias"] != rows[j]["alias"] {
			return rows[i]["alias"].(string) < rows[j]["alias"].(string)
		}
		return rows[i]["index"].(string) < rows[j]["index"].(string)
	})
	return rows
}
