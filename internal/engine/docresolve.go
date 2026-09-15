package engine

import (
	"net/http"
	"sort"
	"strings"
)

// resolveDocIndex resolves the target of a single-document read (GET,
// HEAD, _source, a multi-get item): a concrete index, or an alias pointing
// at exactly one index.
func (c *Cluster) resolveDocIndex(name string) (*Index, error) {
	if ix, ok := c.indices[name]; ok {
		if ix.stateClosed {
			return nil, errIndexClosed(ix)
		}
		if err := c.checkBlock(ix, blockRead); err != nil {
			return nil, err
		}
		return ix, nil
	}
	if name == "_all" {
		e := errIndexNotFound(name)
		e.Reason = "no such index [_all] and no indices exist"
		return nil, docIndexNotFound(e)
	}
	ts := c.aliasTargets(name)
	switch len(ts) {
	case 0:
		return nil, docIndexNotFound(errIndexNotFound(name))
	case 1:
		if err := c.checkBlock(ts[0].ix, blockRead); err != nil {
			return nil, err
		}
		return ts[0].ix, nil
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.ix.Name)
	}
	sort.Strings(names)
	return nil, errIllegalArgument("alias [%s] has more than one index associated with it [%s], can't execute a single index op", name, strings.Join(names, ", "))
}

// requireAliasFailure is the failure of a write with require_alias=true to a
// name that is not an alias.
func (c *Cluster) requireAliasFailure(name string) error {
	if len(c.aliasTargets(name)) > 0 {
		return nil
	}
	return &Error{Status: http.StatusNotFound, Type: "index_not_found_exception",
		Reason: "no such index [" + name + "] and [require_alias] request flag is [true] and [" + name + "] is not an alias", Index: name}
}

// requireRouting fails an operation without routing on an index whose
// mapping requires it.
func requireRouting(ix *Index, id, routing string) error {
	if routing == "" && getBool(getMap(ix.Mapping.Extra, "_routing"), "required", false) {
		return &Error{Status: http.StatusBadRequest, Type: "routing_missing_exception",
			Reason: "routing is required for [" + ix.Name + "]/[" + id + "]", Index: ix.Name}
	}
	return nil
}

// writeIndexSettings returns the settings that apply to a write into name:
// the write index's, or those of the templates an auto-created index would
// get.
func (c *Cluster) writeIndexSettings(name string) M {
	if ix, ok := c.indices[name]; ok {
		return ix.Settings
	}
	if ix, err := c.resolveWriteIndex(name); err == nil {
		return ix.Settings
	}
	var best *Template
	for _, t := range c.templates {
		if t.matches(name) && (best == nil || t.Priority > best.Priority) {
			best = t
		}
	}
	if best != nil {
		return best.Settings
	}
	var legacy []*Template
	for _, t := range c.legacyTemplates {
		if t.matches(name) {
			legacy = append(legacy, t)
		}
	}
	sort.Slice(legacy, func(i, j int) bool { return legacy[i].Priority < legacy[j].Priority })
	merged := M{}
	for _, t := range legacy {
		deepMerge(merged, t.Settings)
	}
	return merged
}

// resolvedPipelines is IngestService.resolvePipelines: the request pipeline
// (or the index's default pipeline) and the index's final pipeline; "_none"
// means no pipeline.
func (c *Cluster) resolvedPipelines(name string, requestPipeline *string) (string, string) {
	idx := getMap(c.writeIndexSettings(name), "index")
	pipeline, final := "_none", "_none"
	if v, ok := idx["default_pipeline"].(string); ok {
		pipeline = v
	}
	if v, ok := idx["final_pipeline"].(string); ok {
		final = v
	}
	if requestPipeline != nil {
		pipeline = *requestPipeline
	}
	return pipeline, final
}

// hasPipelines reports whether a write would run through ingest.
func (c *Cluster) hasPipelines(name string, requestPipeline *string) bool {
	p, f := c.resolvedPipelines(name, requestPipeline)
	return p != "_none" || f != "_none"
}

// pipelineFailure is the ingest failure of a write: osmem has no ingest
// pipelines, so every referenced pipeline does not exist.
func (c *Cluster) pipelineFailure(name string, requestPipeline *string) error {
	p, f := c.resolvedPipelines(name, requestPipeline)
	if p != "_none" {
		return errIllegalArgument("pipeline with id [%s] does not exist", p)
	}
	if f != "_none" {
		return errIllegalArgument("pipeline with id [%s] does not exist", f)
	}
	return nil
}
