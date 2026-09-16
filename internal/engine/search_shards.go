package engine

import "sort"

// SearchShards implements GET/POST /_search_shards and
// GET/POST /{index}/_search_shards (ClusterSearchShardsRequest): the set of
// shards a search against expr would hit. Resolution is lenient (a missing
// index or alias answers with empty shards rather than a 404), matching
// ClusterSearchShardsRequest's default indicesOptions. osmem is single
// node, so every primary shard is modeled as STARTED on that one node
// (shardRoutingJSON, shared with _cluster/state's routing_table); replicas
// stay unassigned and unsearchable, exactly as cluster health accounts them.
func (c *Cluster) SearchShards(expr string, body M, p Params, transportAddress string) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var slice *sliceSpec
	if _, has := body["slice"]; has {
		s, err := parseSlice(body, "slice")
		if err != nil {
			return fail(err)
		}
		slice = s
	}

	o, err := lenientExpandOpenOptions.withParams(p)
	if err != nil {
		return fail(err)
	}
	r := c.newExprResolver()
	exprs := splitCommaJava(expr)
	indices, err := r.concrete(exprs, o)
	if err != nil {
		return fail(err)
	}
	// the alias names an index was reached through, for the indices section
	// below; resolved the same lenient way a search looks up alias filters
	resolved, _ := r.expand(exprs, lenientExpandOpenOptions, true)

	ts := make([]target, len(indices))
	for i, ix := range indices {
		ts[i] = target{ix: ix}
	}
	prefShards, err := preferenceShards(p.Get("preference"), ts)
	if err != nil {
		return fail(err)
	}
	routing := splitList(p.Get("routing"))

	type shardCandidate struct {
		ix    *Index
		shard int
	}
	var candidates []shardCandidate
	for _, ix := range indices {
		n := indexShardCount(ix)
		allowedRouting := map[int]bool{}
		for _, rt := range routing {
			allowedRouting[shardOf(ix, rt)] = true
		}
		for s := 0; s < n; s++ {
			if len(routing) > 0 && !allowedRouting[s] {
				continue
			}
			if prefShards != nil && !prefShards[ix.Name][s] {
				continue
			}
			candidates = append(candidates, shardCandidate{ix, s})
		}
	}
	if slice != nil {
		// _search_shards slices the resulting list of shards positionally
		// (splitting the shard set among slice workers), unlike a scroll's
		// slice which hashes documents within each shard (sliceSelection).
		kept := candidates[:0]
		for i, cd := range candidates {
			if i%slice.max == slice.id {
				kept = append(kept, cd)
			}
		}
		candidates = kept
	}

	shardsOut := make([]any, 0, len(candidates))
	for _, cd := range candidates {
		shardsOut = append(shardsOut, []any{shardRoutingJSON(cd.ix, cd.shard, true)})
	}

	indicesOut := M{}
	for _, ix := range indices {
		entry := M{}
		if aliases := matchingAliasNames(ix, resolved); len(aliases) > 0 {
			names := make([]any, len(aliases))
			for i, name := range aliases {
				names[i] = name
			}
			entry["aliases"] = names
			if filterNames := filteringAliases(ix, resolved); len(filterNames) > 0 {
				entry["filter"] = echoedAliasFilter(ix, filterNames)
			}
		}
		indicesOut[ix.Name] = entry
	}

	nodes := M{}
	if len(candidates) > 0 {
		nodes[osmemNodeID] = M{"name": osmemNodeID, "ephemeral_id": osmemNodeID, "transport_address": transportAddress, "attributes": M{}}
	}
	return ok(M{"nodes": nodes, "indices": indicesOut, "shards": shardsOut})
}

// matchingAliasNames is the sorted list of alias names in resolved
// (filteringAliases's input) that reference ix, excluding ix's own name:
// the "aliases" reported for an index depend on how the request expression
// reached it, not on every alias the index happens to have.
func matchingAliasNames(ix *Index, resolved []string) []string {
	var names []string
	for _, name := range resolved {
		if name == ix.Name {
			continue
		}
		if _, ok := ix.Aliases[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// echoedAliasFilter renders the combined filter of filterNames the way
// _search_shards echoes it: aliasFilter's plain M{"bool": {"should": ...}}
// wrapping gets the adjust_pure_negative/boost defaults a real bool query
// carries once parsed, and each leaf is normalized (echoedFilterLeaf).
func echoedAliasFilter(ix *Index, filterNames []string) M {
	if len(filterNames) == 1 {
		return echoedFilterLeaf(ix.Aliases[filterNames[0]].Filter)
	}
	should := make([]any, len(filterNames))
	for i, name := range filterNames {
		should[i] = echoedFilterLeaf(ix.Aliases[name].Filter)
	}
	return M{"bool": M{"should": should, "adjust_pure_negative": true, "boost": Float(1)}}
}

// echoedFilterLeaf renders a stored alias filter the way OpenSearch's
// _search_shards echoes it: a filter is parsed once into a QueryBuilder
// when the alias is created, and toXContent always writes every field with
// its default (e.g. boost 1.0) even when the user wrote the compact form.
// osmem approximates this for the term queries alias filters commonly use;
// other query types are echoed as stored (a documented follow-up).
func echoedFilterLeaf(raw M) M {
	body, ok := raw["term"].(M)
	if len(raw) != 1 || !ok || len(body) != 1 {
		return raw
	}
	for field, v := range body {
		if m, isObj := v.(M); isObj {
			out := cloneDeep(m).(M)
			if _, has := out["boost"]; !has {
				out["boost"] = Float(1)
			}
			return M{"term": M{field: out}}
		}
		return M{"term": M{field: M{"value": v, "boost": Float(1)}}}
	}
	return raw
}
