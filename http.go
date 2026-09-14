package osmem

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/shibukawa/osmem/internal/engine"
)

type M = map[string]any

type handlerFunc func(h *httpHandler, r *http.Request, vars map[string]string, body []byte) (engine.Response, error)

type route struct {
	methods map[string]bool
	pattern []string
	fn      handlerFunc
}

type httpHandler struct {
	c      *engine.Cluster
	routes []route
}

func newHTTPHandler(c *engine.Cluster) *httpHandler {
	h := &httpHandler{c: c}
	h.routes = buildRoutes()
	return h
}

func buildRoutes() []route {
	var routes []route
	add := func(methods, pattern string, fn handlerFunc) {
		ms := map[string]bool{}
		for _, m := range strings.Split(methods, ",") {
			ms[m] = true
		}
		segs := strings.Split(strings.Trim(pattern, "/"), "/")
		if pattern == "/" {
			segs = nil
		}
		routes = append(routes, route{methods: ms, pattern: segs, fn: fn})
	}
	ack := func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Acknowledge(v["index"], params(r))
	}
	acked := func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Acknowledged(v["index"], params(r))
	}
	// cluster level
	add("GET", "/", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Info()
	})
	add("HEAD", "/", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return engine.Response{Status: 200}, nil
	})
	add("GET", "/_cluster/health", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Health("", params(r))
	})
	add("GET", "/_cluster/health/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Health(v["index"], params(r))
	})
	add("GET", "/_cluster/settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClusterSettings()
	})
	add("PUT", "/_cluster/settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutClusterSettings(m)
	})
	add("GET", "/_cluster/state", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return engine.Response{Status: 200, Body: M{"cluster_name": h.c.Name, "cluster_uuid": "osmem-cluster", "version": 1, "state_uuid": "osmem", "master_node": "osmem-node", "cluster_manager_node": "osmem-node",
			"nodes": M{"osmem-node": M{"name": "osmem-node", "ephemeral_id": "osmem-node", "transport_address": h.address(), "attributes": M{}}}}}, nil
	})
	add("GET", "/_cluster/stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return engine.Response{Status: 200, Body: M{"cluster_name": h.c.Name, "status": "green", "indices": M{"count": len(h.c.Indices())}, "nodes": M{"count": M{"total": 1}}}}, nil
	})
	add("GET", "/_nodes", nodesInfo)
	add("GET", "/_nodes/{a}", nodesInfo)
	add("GET", "/_nodes/{a}/{b}", nodesInfo)
	add("GET", "/_cat/indices", catIndices)
	add("GET", "/_cat/indices/{index}", catIndices)
	add("GET", "/_cat/aliases", catAliases)
	add("GET", "/_cat/aliases/{name}", catAliases)
	add("GET", "/_cat/health", catHealth)
	add("GET", "/_cat/count", catCount)
	add("GET", "/_cat/count/{index}", catCount)
	add("GET", "/_cat/nodes", catNodes)
	add("GET", "/_cat/master", catMaster)
	add("GET", "/_cat/cluster_manager", catMaster)
	add("GET", "/_cat/plugins", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return catResponse(r, nil, []string{"name", "component", "version"})
	})
	add("GET", "/_cat/templates", catTemplates)
	add("GET", "/_cat/templates/{name}", catTemplates)
	add("GET", "/_cat", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return engine.Response{Status: 200, Body: rawText("=^.^=\n/_cat/indices\n/_cat/aliases\n/_cat/health\n/_cat/count\n")}, nil
	})
	// aliases
	add("POST", "/_aliases", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.UpdateAliases(m)
	})
	add("GET", "/_alias", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetAliases("", "", params(r))
	})
	add("GET", "/_aliases", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetAliases("", "", params(r))
	})
	add("GET", "/_alias/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetAliases("", v["name"], params(r))
	})
	add("HEAD", "/_alias/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.AliasExists("", v["name"], params(r))
	})
	// templates
	add("PUT,POST", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutIndexTemplate(v["name"], m)
	})
	add("GET", "/_index_template", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndexTemplate("")
	})
	add("GET", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndexTemplate(v["name"])
	})
	add("HEAD", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexTemplateExists(v["name"])
	})
	add("DELETE", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteIndexTemplate(v["name"])
	})
	add("PUT,POST", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutLegacyTemplate(v["name"], m)
	})
	add("GET", "/_template", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetLegacyTemplate("")
	})
	add("GET", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetLegacyTemplate(v["name"])
	})
	add("HEAD", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.LegacyTemplateExists(v["name"])
	})
	add("DELETE", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteLegacyTemplate(v["name"])
	})
	// documents and search, cluster wide
	add("POST,PUT", "/_bulk", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Bulk("", body, params(r))
	})
	add("GET,POST", "/_search", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Search("", m, params(r))
	})
	add("GET,POST", "/_count", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Count("", m, params(r))
	})
	add("GET,POST", "/_msearch", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.MultiSearch("", body, params(r))
	})
	add("GET,POST", "/_mget", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiGet("", m, params(r))
	})
	add("GET,POST", "/_search/scroll", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Scroll(m, params(r))
	})
	add("GET,POST", "/_search/scroll/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		m["scroll_id"] = v["id"]
		return h.c.Scroll(m, params(r))
	})
	add("DELETE", "/_search/scroll", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.ClearScroll(m, "")
	})
	add("DELETE", "/_search/scroll/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClearScroll(nil, v["id"])
	})
	add("DELETE", "/_search/point_in_time", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.DeletePIT(m, false)
	})
	add("DELETE", "/_search/point_in_time/_all", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeletePIT(nil, true)
	})
	add("GET", "/_search/point_in_time/_all", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ListPITs()
	})
	add("POST", "/_refresh", ack)
	add("GET", "/_refresh", ack)
	add("POST", "/_flush", ack)
	add("POST", "/_forcemerge", ack)
	add("POST", "/_reindex", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Reindex(m, params(r))
	})
	add("GET,POST", "/_analyze", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Analyze("", m)
	})
	add("GET", "/_mapping", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetMapping("", params(r))
	})
	add("GET", "/_settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetSettings("", params(r))
	})
	add("GET", "/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexStats("", params(r))
	})
	add("GET", "/_all", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndex("_all", params(r))
	})
	// index level
	add("PUT", "/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.CreateIndex(v["index"], m)
	})
	add("HEAD", "/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexExists(v["index"], params(r))
	})
	add("GET", "/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndex(v["index"], params(r))
	})
	add("DELETE", "/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteIndex(v["index"], params(r))
	})
	add("GET", "/{index}/_mapping", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetMapping(v["index"], params(r))
	})
	add("PUT,POST", "/{index}/_mapping", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutMapping(v["index"], m, params(r))
	})
	add("GET", "/{index}/_mapping/field/{fields}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetFieldMapping(v["index"], v["fields"], params(r))
	})
	add("GET", "/_mapping/field/{fields}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetFieldMapping("", v["fields"], params(r))
	})
	add("GET", "/{index}/_settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetSettings(v["index"], params(r))
	})
	add("GET", "/{index}/_settings/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		p := params(r)
		p["settings_filter"] = v["name"]
		return h.c.GetSettings(v["index"], p)
	})
	add("PUT", "/{index}/_settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutSettings(v["index"], m, params(r))
	})
	add("GET", "/{index}/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexStats(v["index"], params(r))
	})
	add("GET", "/{index}/_stats/{metric}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexStats(v["index"], params(r))
	})
	add("POST,GET", "/{index}/_refresh", ack)
	add("POST", "/{index}/_flush", ack)
	add("POST", "/{index}/_forcemerge", ack)
	add("POST", "/{index}/_cache/clear", ack)
	add("POST", "/{index}/_open", acked)
	add("POST", "/{index}/_close", acked)
	add("PUT,POST", "/{index}/_doc/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		dp, err := docParams(r)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.IndexDoc(v["index"], v["id"], body, dp)
	})
	add("POST", "/{index}/_doc", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		dp, err := docParams(r)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.IndexDoc(v["index"], "", body, dp)
	})
	add("PUT,POST", "/{index}/_create/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		dp, err := docParams(r)
		if err != nil {
			return engine.Response{}, err
		}
		dp.OpType = "create"
		return h.c.IndexDoc(v["index"], v["id"], body, dp)
	})
	add("GET", "/{index}/_doc/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetDoc(v["index"], v["id"], params(r))
	})
	add("HEAD", "/{index}/_doc/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DocExists(v["index"], v["id"], params(r))
	})
	add("DELETE", "/{index}/_doc/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		dp, err := docParams(r)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.DeleteDoc(v["index"], v["id"], dp)
	})
	add("POST", "/{index}/_update/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.UpdateDoc(v["index"], v["id"], m, params(r))
	})
	add("GET", "/{index}/_source/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetSource(v["index"], v["id"], params(r))
	})
	add("HEAD", "/{index}/_source/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DocExists(v["index"], v["id"], params(r))
	})
	add("GET,POST", "/{index}/_search", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Search(v["index"], m, params(r))
	})
	add("POST", "/{index}/_search/point_in_time", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.CreatePIT(v["index"], params(r))
	})
	add("GET,POST", "/{index}/_count", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Count(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_msearch", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.MultiSearch(v["index"], body, params(r))
	})
	add("GET,POST", "/{index}/_mget", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiGet(v["index"], m, params(r))
	})
	add("POST,PUT", "/{index}/_bulk", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Bulk(v["index"], body, params(r))
	})
	add("POST", "/{index}/_delete_by_query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.DeleteByQuery(v["index"], m, params(r))
	})
	add("POST", "/{index}/_update_by_query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.UpdateByQuery(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_analyze", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Analyze(v["index"], m)
	})
	for _, seg := range []string{"_alias", "_aliases"} {
		add("PUT,POST", "/{index}/"+seg+"/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			m, err := decodeBody(body)
			if err != nil {
				return engine.Response{}, err
			}
			return h.c.PutAlias(v["index"], v["name"], m)
		})
		add("DELETE", "/{index}/"+seg+"/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			return h.c.DeleteAlias(v["index"], v["name"])
		})
		add("GET", "/{index}/"+seg+"/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			return h.c.GetAliases(v["index"], v["name"], params(r))
		})
		add("HEAD", "/{index}/"+seg+"/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			return h.c.AliasExists(v["index"], v["name"], params(r))
		})
		add("GET", "/{index}/"+seg, func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			return h.c.GetAliases(v["index"], "", params(r))
		})
	}
	return routes
}

// rawText marks a plain-text response body.
type rawText string

func params(r *http.Request) engine.Params {
	p := engine.Params{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			p[k] = vs[0]
		} else {
			p[k] = ""
		}
	}
	return p
}

func docParams(r *http.Request) (engine.DocParams, error) {
	return engine.DocParamsFrom(params(r))
}

func decodeBody(body []byte) (M, error) {
	return engine.DecodeObject(body)
}

// match finds the route for a request. pathKnown reports whether some
// route matched the path with another method.
func (h *httpHandler) match(method, path string) (fn handlerFunc, vars map[string]string, found bool, pathKnown bool) {
	path = strings.Trim(path, "/")
	var segs []string
	if path != "" {
		segs = strings.Split(path, "/")
	}
	for _, rt := range h.routes {
		if len(rt.pattern) != len(segs) {
			continue
		}
		vars := map[string]string{}
		matched := true
		for i, p := range rt.pattern {
			s := segs[i]
			if strings.HasPrefix(p, "{") {
				name := strings.Trim(p, "{}")
				if name == "index" && strings.HasPrefix(s, "_") && s != "_all" {
					matched = false
					break
				}
				vars[name] = s
				continue
			}
			if p != s {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if !rt.methods[method] {
			pathKnown = true
			continue
		}
		return rt.fn, vars, true, true
	}
	return nil, nil, false, pathKnown
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			writeError(w, r, &engine.Error{Status: 400, Type: "parsing_exception", Reason: "invalid gzip body: " + err.Error()})
			return
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		writeError(w, r, &engine.Error{Status: 400, Type: "parsing_exception", Reason: err.Error()})
		return
	}
	// match on the escaped path so index names containing "%2C" etc. keep
	// their segment boundaries
	path := r.URL.EscapedPath()
	fn, vars, ok, pathKnown := h.match(r.Method, path)
	if !ok {
		if r.Method == "HEAD" {
			w.WriteHeader(404)
			return
		}
		if pathKnown {
			writeError(w, r, &engine.Error{Status: 405, Type: "invalid_method_exception", Reason: fmt.Sprintf("Incorrect HTTP method for uri [%s] and method [%s], allowed: see documentation", r.URL.RequestURI(), r.Method)})
			return
		}
		writeResponse(w, r, engine.Response{Status: 400, Body: M{"error": fmt.Sprintf("no handler found for uri [%s] and method [%s]", r.URL.RequestURI(), r.Method)}})
		return
	}
	for k, v := range vars {
		if u, err := url.PathUnescape(v); err == nil {
			vars[k] = u
		}
	}
	res, err := fn(h, r, vars, body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeResponse(w, r, res)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := err.(*engine.Error)
	if !ok {
		e = &engine.Error{Status: 500, Type: "exception", Reason: err.Error()}
	}
	writeResponse(w, r, engine.Response{Status: e.Status, Body: e.Body()})
}

func writeResponse(w http.ResponseWriter, r *http.Request, res engine.Response) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	if r.Method == "HEAD" || res.Body == nil {
		if res.Body == nil && r.Method != "HEAD" {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		}
		w.WriteHeader(res.Status)
		return
	}
	if txt, ok := res.Body.(rawText); ok {
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(res.Status)
		_, _ = io.WriteString(w, string(txt))
		return
	}
	body := res.Body
	if fp := r.URL.Query().Get("filter_path"); fp != "" {
		body = filterPath(body, strings.Split(fp, ","))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if _, ok := r.URL.Query()["pretty"]; ok && r.URL.Query().Get("pretty") != "false" {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(body); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(500)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"exception","reason":%q},"status":500}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(res.Status)
	_, _ = w.Write(buf.Bytes())
}

// filterPath implements the filter_path parameter (dotted paths with
// wildcards).
func filterPath(body any, paths []string) any {
	raw, err := json.Marshal(body)
	if err != nil {
		return body
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return body
	}
	var out any
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		part := extractPath(tree, strings.Split(p, "."))
		out = mergeTrees(out, part)
	}
	if out == nil {
		return M{}
	}
	return out
}

func extractPath(v any, parts []string) any {
	if len(parts) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			if !engine.WildcardMatch(parts[0], k) {
				continue
			}
			rest := parts[1:]
			if parts[0] == "**" {
				// ** matches any depth
				if sub := extractPath(e, parts); sub != nil {
					out[k] = sub
				}
				if len(rest) > 0 {
					if sub := extractPath(e, rest); sub != nil {
						out[k] = mergeTrees(out[k], sub)
					}
				}
				continue
			}
			sub := extractPath(e, rest)
			if sub == nil {
				continue
			}
			out[k] = sub
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		var out []any
		for _, e := range t {
			sub := extractPath(e, parts)
			if sub != nil {
				out = append(out, sub)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		if len(parts) == 0 {
			return v
		}
		return nil
	}
}

func mergeTrees(a, b any) any {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	am, ok1 := a.(map[string]any)
	bm, ok2 := b.(map[string]any)
	if ok1 && ok2 {
		for k, v := range bm {
			am[k] = mergeTrees(am[k], v)
		}
		return am
	}
	al, ok1 := a.([]any)
	bl, ok2 := b.([]any)
	if ok1 && ok2 && len(al) == len(bl) {
		for i := range al {
			al[i] = mergeTrees(al[i], bl[i])
		}
		return al
	}
	return b
}

// cat APIs ---------------------------------------------------------------

func catResponse(r *http.Request, rows []M, columns []string) (engine.Response, error) {
	q := r.URL.Query()
	if hs := q.Get("h"); hs != "" {
		columns = strings.Split(hs, ",")
	}
	if q.Get("format") == "json" {
		list := make([]any, 0, len(rows))
		for _, row := range rows {
			m := M{}
			for _, c := range columns {
				m[c] = row[c]
			}
			list = append(list, m)
		}
		return engine.Response{Status: 200, Body: list}, nil
	}
	// text table
	widths := make([]int, len(columns))
	_, verbose := q["v"]
	if verbose {
		for i, c := range columns {
			widths[i] = len(c)
		}
	}
	cells := make([][]string, len(rows))
	for ri, row := range rows {
		cells[ri] = make([]string, len(columns))
		for i, c := range columns {
			s := fmt.Sprint(row[c])
			if row[c] == nil {
				s = ""
			}
			cells[ri][i] = s
			if len(s) > widths[i] {
				widths[i] = len(s)
			}
		}
	}
	var sb strings.Builder
	writeRow := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(v)
			if i < len(vals)-1 {
				sb.WriteString(strings.Repeat(" ", widths[i]-len(v)))
			}
		}
		sb.WriteByte('\n')
	}
	if verbose {
		writeRow(columns)
	}
	for _, row := range cells {
		writeRow(row)
	}
	return engine.Response{Status: 200, Body: rawText(sb.String())}, nil
}

func catIndices(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	res, err := h.c.IndexStats(v["index"], params(r))
	if err != nil {
		return engine.Response{}, err
	}
	stats := res.Body.(map[string]any)["indices"].(map[string]any)
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []M
	for _, n := range names {
		st := stats[n].(map[string]any)
		count := st["primaries"].(map[string]any)["docs"].(map[string]any)["count"]
		settings, _ := h.c.GetSettings(n, engine.Params{})
		indexSettings := settings.Body.(map[string]any)[n].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)
		health, _ := h.c.Health(n, params(r))
		healthStatus := health.Body.(map[string]any)["status"]
		rows = append(rows, M{
			"health": healthStatus, "status": "open", "index": n, "uuid": st["uuid"], "pri": fmt.Sprint(indexSettings["number_of_shards"]), "rep": fmt.Sprint(indexSettings["number_of_replicas"]),
			"docs.count": fmt.Sprint(count), "docs.deleted": "0", "store.size": "0b", "pri.store.size": "0b",
		})
	}
	return catResponse(r, rows, []string{"health", "status", "index", "uuid", "pri", "rep", "docs.count", "docs.deleted", "store.size", "pri.store.size"})
}

func catAliases(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	rows := h.c.CatAliases(v["name"])
	out := make([]M, len(rows))
	for i, row := range rows {
		out[i] = M(row)
	}
	return catResponse(r, out, []string{"alias", "index", "filter", "routing.index", "routing.search", "is_write_index"})
}

func catHealth(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	res, _ := h.c.Health("", params(r))
	hm := res.Body.(map[string]any)
	row := M{"epoch": "0", "timestamp": "00:00:00", "cluster": hm["cluster_name"], "status": hm["status"], "node.total": "1", "node.data": "1", "discovered_cluster_manager": "true",
		"shards": fmt.Sprint(hm["active_shards"]), "pri": fmt.Sprint(hm["active_primary_shards"]), "relo": "0", "init": "0", "unassign": fmt.Sprint(hm["unassigned_shards"]), "pending_tasks": "0", "max_task_wait_time": "-", "active_shards_percent": fmt.Sprintf("%.1f%%", hm["active_shards_percent_as_number"])}
	return catResponse(r, []M{row}, []string{"epoch", "timestamp", "cluster", "status", "node.total", "node.data", "discovered_cluster_manager", "shards", "pri", "relo", "init", "unassign", "pending_tasks", "max_task_wait_time", "active_shards_percent"})
}

func catCount(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	res, err := h.c.Count(v["index"], nil, params(r))
	if err != nil {
		return engine.Response{}, err
	}
	row := M{"epoch": "0", "timestamp": "00:00:00", "count": fmt.Sprint(res.Body.(map[string]any)["count"])}
	return catResponse(r, []M{row}, []string{"epoch", "timestamp", "count"})
}

// address is the HTTP address reported to clients that sniff nodes.
func (h *httpHandler) address() string {
	if h.c.HTTPAddress != "" {
		return h.c.HTTPAddress
	}
	return "127.0.0.1:9200"
}

func catNodes(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	ip := h.address()
	if idx := strings.LastIndex(ip, ":"); idx > 0 {
		ip = ip[:idx]
	}
	row := M{"ip": ip, "heap.percent": "0", "ram.percent": "0", "cpu": "0", "load_1m": "0", "load_5m": "0", "load_15m": "0", "node.role": "dimr", "node.roles": "data,ingest,cluster_manager,remote_cluster_client", "cluster_manager": "*", "master": "*", "name": "osmem-node"}
	return catResponse(r, []M{row}, []string{"ip", "heap.percent", "ram.percent", "cpu", "load_1m", "load_5m", "load_15m", "node.role", "node.roles", "cluster_manager", "name"})
}

func catMaster(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	row := M{"id": "osmem-node", "host": "127.0.0.1", "ip": "127.0.0.1", "node": "osmem-node"}
	return catResponse(r, []M{row}, []string{"id", "host", "ip", "node"})
}

func catTemplates(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	res, err := h.c.GetIndexTemplate(v["name"])
	var rows []M
	if err == nil {
		for _, t := range res.Body.(map[string]any)["index_templates"].([]any) {
			tm := t.(map[string]any)
			it := tm["index_template"].(map[string]any)
			pats, _ := json.Marshal(it["index_patterns"])
			rows = append(rows, M{"name": tm["name"], "index_patterns": string(pats), "order": fmt.Sprint(it["priority"]), "version": fmt.Sprint(it["version"]), "composed_of": "[]"})
		}
	}
	return catResponse(r, rows, []string{"name", "index_patterns", "order", "version", "composed_of"})
}

func nodesInfo(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	return engine.Response{Status: 200, Body: M{
		"_nodes": M{"total": 1, "successful": 1, "failed": 0}, "cluster_name": h.c.Name,
		"nodes": M{"osmem-node": M{"name": "osmem-node", "transport_address": h.address(), "host": "127.0.0.1", "ip": "127.0.0.1", "version": engine.Version, "build_type": "tar", "roles": []any{"cluster_manager", "data", "ingest"},
			"http": M{"publish_address": h.address(), "bound_address": []any{h.address()}}}},
	}}, nil
}
