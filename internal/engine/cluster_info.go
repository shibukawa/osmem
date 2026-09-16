package engine

import (
	"sort"
	"strconv"
)

var clusterStateMetrics = []string{"version", "master_node", "cluster_manager_node", "blocks", "nodes", "metadata", "routing_table", "routing_nodes"}

// ClusterState implements GET /_cluster/state[/{metric}[/{index}]] for the
// single node model. Unknown metrics are ignored like in OpenSearch; missing
// indices are left out (lenient index options); flat_settings applies to
// index and template settings.
func (c *Cluster) ClusterState(metric, expr string, p Params, transportAddress string) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	metrics := map[string]bool{}
	for _, m := range splitList(metric) {
		metrics[m] = true
	}
	if metric == "" || metrics["_all"] {
		for _, m := range clusterStateMetrics {
			metrics[m] = true
		}
	}
	var indices []*Index
	if expr == "" || expr == "_all" || expr == "*" {
		for _, name := range c.sortedIndexNames() {
			indices = append(indices, c.indices[name])
		}
	} else {
		ts, err := c.resolve(expr, resolveOptions{params: p, base: &lenientExpandOpenOptions})
		if err != nil {
			return fail(err)
		}
		indices = uniqueTargetIndices(ts)
	}
	out := M{"cluster_name": c.Name, "cluster_uuid": "osmem-cluster"}
	if metrics["version"] {
		out["version"] = 1
		out["state_uuid"] = "osmem"
	}
	if metrics["master_node"] {
		out["master_node"] = osmemNodeID
	}
	if metrics["cluster_manager_node"] {
		out["cluster_manager_node"] = osmemNodeID
	}
	if metrics["blocks"] {
		out["blocks"] = c.clusterBlocksJSON(indices)
	}
	if metrics["nodes"] {
		out["nodes"] = M{osmemNodeID: M{"name": osmemNodeID, "ephemeral_id": osmemNodeID, "transport_address": transportAddress, "attributes": M{}}}
	}
	if metrics["metadata"] {
		out["metadata"] = c.clusterMetadataJSON(indices, p)
	}
	if metrics["routing_table"] {
		out["routing_table"] = M{"indices": routingTableJSON(indices)}
	}
	if metrics["routing_nodes"] {
		out["routing_nodes"] = routingNodesJSON(indices)
	}
	return ok(out)
}

func blockJSON(b clusterBlock) M {
	levels := make([]any, len(b.levels))
	for i, l := range b.levels {
		levels[i] = l
	}
	retryable := b.id == 12 || b.id == 13
	return M{"description": b.description, "retryable": retryable, "levels": levels}
}

func (c *Cluster) clusterBlocksJSON(indices []*Index) M {
	out := M{}
	global := M{}
	for _, b := range globalBlockDefs {
		for _, scope := range []string{"transient", "persistent"} {
			if v, ok := c.clusterSettings[scope].(M)[b.setting]; ok {
				if settingString(v) == "true" {
					global[strconv.Itoa(b.id)] = blockJSON(b)
				}
				break
			}
		}
	}
	if len(global) > 0 {
		out["global"] = global
	}
	byIndex := M{}
	for _, ix := range indices {
		blocks := M{}
		for _, b := range indexBlockDefs {
			if v, ok := getNested(ix.Settings, b.setting); ok && settingString(v) == "true" {
				blocks[strconv.Itoa(b.id)] = blockJSON(b)
			}
		}
		if len(blocks) > 0 {
			byIndex[ix.Name] = blocks
		}
	}
	if len(byIndex) > 0 {
		out["indices"] = byIndex
	}
	return out
}

func allocationID(ix *Index, shard int) string {
	return ix.UUID + "-" + strconv.Itoa(shard)
}

func (c *Cluster) clusterMetadataJSON(indices []*Index, p Params) M {
	flatOut := p.Bool("flat_settings", false)
	templates := M{}
	for name, t := range c.legacyTemplates {
		mappings := M{}
		if len(t.Mappings) > 0 {
			mappings = M{"_doc": t.Mappings}
		}
		templates[name] = M{"order": t.Priority, "index_patterns": t.Patterns, "settings": renderSettings(flatIndexSettings(t.Settings), flatOut),
			"mappings": mappings, "aliases": renderTemplateAliases(t.Aliases, true)}
	}
	indicesOut := M{}
	for _, ix := range indices {
		shards := indexShardCount(ix)
		primaryTerms := M{}
		inSync := M{}
		activeIDs := make([]any, shards)
		for s := 0; s < shards; s++ {
			primaryTerms[strconv.Itoa(s)] = 1
			inSync[strconv.Itoa(s)] = []any{allocationID(ix, s)}
			activeIDs[s] = s
		}
		aliasNames := make([]string, 0, len(ix.Aliases))
		for name := range ix.Aliases {
			aliasNames = append(aliasNames, name)
		}
		sort.Strings(aliasNames)
		aliases := make([]any, len(aliasNames))
		for i, name := range aliasNames {
			aliases[i] = name
		}
		mappings := M{}
		if m := ix.Mapping.toJSON(); len(m) > 0 {
			mappings = M{"_doc": m}
		}
		indicesOut[ix.Name] = M{
			"version": 1, "mapping_version": 1, "settings_version": 1, "aliases_version": 1,
			"routing_num_shards": catRoutingShards(getMap(ix.Settings, "index"), shards),
			"state":              indexStateName(ix),
			"settings":           renderSettings(flatIndexSettings(ix.Settings), flatOut),
			"mappings":           mappings,
			"aliases":            aliases,
			"primary_terms":      primaryTerms, "primary_terms_map": cloneDeep(primaryTerms),
			"in_sync_allocations": inSync, "rollover_info": M{}, "system": false,
			"ingestion_status": M{"is_paused": false},
			"split_shards_metadata": M{"num_of_root_shards": shards, "max_shard_id": shards - 1, "active_shard_ids": activeIDs,
				"root_shards_to_all_children": M{}, "parent_to_child_shards": M{}},
		}
	}
	out := M{
		"cluster_uuid": "osmem-cluster", "cluster_uuid_committed": false,
		"cluster_coordination": M{"term": 1, "last_committed_config": []any{osmemNodeID}, "last_accepted_config": []any{osmemNodeID}, "voting_config_exclusions": []any{}},
		"templates":            templates, "indices": indicesOut, "index-graveyard": M{"tombstones": []any{}},
	}
	if len(c.templates) > 0 {
		composable := M{}
		for name, t := range c.templates {
			composable[name] = composableTemplateJSON(t)
		}
		out["index_template"] = M{"index_template": composable}
	}
	if len(c.componentTemplates) > 0 {
		components := M{}
		for name, ct := range c.componentTemplates {
			components[name] = componentTemplateJSON(ct)
		}
		out["component_template"] = M{"component_template": components}
	}
	return out
}

func shardRoutingJSON(ix *Index, shard int, primary bool) M {
	if primary {
		return M{"state": "STARTED", "primary": true, "searchOnly": false, "node": osmemNodeID, "relocating_node": nil, "shard": shard,
			"index": ix.Name, "allocation_id": M{"id": allocationID(ix, shard)}}
	}
	return M{"state": "UNASSIGNED", "primary": false, "searchOnly": false, "node": nil, "relocating_node": nil, "shard": shard, "index": ix.Name,
		"recovery_source": M{"type": "PEER"},
		"unassigned_info": M{"reason": "INDEX_CREATED", "at": ix.Created.UTC().Format("2006-01-02T15:04:05.000Z"), "delayed": false, "allocation_status": "no_attempt"}}
}

func routingTableJSON(indices []*Index) M {
	out := M{}
	for _, ix := range indices {
		shards := M{}
		for s := 0; s < indexShardCount(ix); s++ {
			copies := []any{shardRoutingJSON(ix, s, true)}
			for r := 0; r < indexReplicaCount(ix); r++ {
				copies = append(copies, shardRoutingJSON(ix, s, false))
			}
			shards[strconv.Itoa(s)] = copies
		}
		out[ix.Name] = M{"shards": shards}
	}
	return out
}

func routingNodesJSON(indices []*Index) M {
	unassigned := []any{}
	started := []any{}
	for _, ix := range indices {
		for s := 0; s < indexShardCount(ix); s++ {
			started = append(started, shardRoutingJSON(ix, s, true))
			for r := 0; r < indexReplicaCount(ix); r++ {
				unassigned = append(unassigned, shardRoutingJSON(ix, s, false))
			}
		}
	}
	return M{"unassigned": unassigned, "nodes": M{osmemNodeID: started}}
}

// ClusterStats implements GET /_cluster/stats with the counters osmem can
// derive; node level resource statistics are zero.
func (c *Cluster) ClusterStats(p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := "green"
	primaries, docs := 0, 0
	size := int64(0)
	minShards, maxShards := 0, 0
	for i, name := range c.sortedIndexNames() {
		ix := c.indices[name]
		shards := indexShardCount(ix)
		primaries += shards
		if ix.stateClosed {
			status = "red"
		} else if indexReplicaCount(ix) > 0 && status == "green" {
			status = "yellow"
		}
		for _, s := range catIndexInfo(ix).Shards {
			docs += s.Docs
			size += s.Bytes
		}
		if i == 0 || shards < minShards {
			minShards = shards
		}
		if shards > maxShards {
			maxShards = shards
		}
	}
	shardsSection := M{}
	if n := len(c.indices); n > 0 {
		avg := Double(float64(primaries) / float64(n))
		shardsSection = M{"total": primaries, "primaries": primaries, "replication": Double(0),
			"index": M{"shards": M{"min": minShards, "max": maxShards, "avg": avg}, "primaries": M{"min": minShards, "max": maxShards, "avg": avg},
				"replication": M{"min": Double(0), "max": Double(0), "avg": Double(0)}}}
	}
	return ok(M{
		"_nodes":       M{"total": 1, "successful": 1, "failed": 0},
		"cluster_name": c.Name, "cluster_uuid": "osmem-cluster", "timestamp": c.now().UnixMilli(), "status": status,
		"indices": M{
			"count": len(c.indices), "shards": shardsSection,
			"docs":        M{"count": docs, "deleted": 0},
			"store":       M{"size_in_bytes": size, "reserved_in_bytes": 0},
			"fielddata":   M{"memory_size_in_bytes": 0, "evictions": 0},
			"query_cache": M{"memory_size_in_bytes": 0, "total_count": 0, "hit_count": 0, "miss_count": 0, "cache_size": 0, "cache_count": 0, "evictions": 0},
			"completion":  M{"size_in_bytes": 0},
			"segments":    newStatsSections()["segments"],
			"mappings":    M{"field_types": []any{}},
			"analysis": M{"char_filter_types": []any{}, "tokenizer_types": []any{}, "filter_types": []any{}, "analyzer_types": []any{},
				"built_in_char_filters": []any{}, "built_in_tokenizers": []any{}, "built_in_filters": []any{}, "built_in_analyzers": []any{}},
		},
		"nodes": M{
			"count":    M{"total": 1, "cluster_manager": 1, "coordinating_only": 0, "data": 1, "ingest": 1, "master": 1, "remote_cluster_client": 1, "search": 0, "warm": 0},
			"versions": []any{Version},
			"os":       M{"available_processors": 1, "allocated_processors": 1, "names": []any{}, "pretty_names": []any{}, "mem": M{"total_in_bytes": 0, "free_in_bytes": 0, "used_in_bytes": 0, "free_percent": 0, "used_percent": 0}},
			"process":  M{"cpu": M{"percent": 0}, "open_file_descriptors": M{"min": 0, "max": 0, "avg": 0}},
			"jvm":      M{"max_uptime_in_millis": 0, "versions": []any{}, "mem": M{"heap_used_in_bytes": 0, "heap_max_in_bytes": 0}, "threads": 0},
			"fs":       M{"total_in_bytes": 0, "free_in_bytes": 0, "available_in_bytes": 0, "cache_reserved_in_bytes": 0},
			"plugins":  []any{}, "network_types": M{"transport_types": M{}, "http_types": M{}}, "discovery_types": M{"single-node": 1},
			"packaging_types": []any{M{"type": "tar", "count": 1}}, "ingest": M{"number_of_pipelines": 0, "processor_stats": M{}},
		},
	})
}

// indexStateName is IndexMetadata.State in its rendered form.
func indexStateName(ix *Index) string {
	if ix.stateClosed {
		return "close"
	}
	return "open"
}
