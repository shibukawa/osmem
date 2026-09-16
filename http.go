package osmem

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

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
	router *router
}

// sharedRoutes and sharedRouter hold the route table once for the process.
// Every handler closure takes the *httpHandler as its first argument and
// reaches the cluster through h.c, so nothing in buildRoutes or the router
// trie is specific to one Cluster; New and Clone (which otherwise rebuild
// this several-hundred-route table on every call) can share it safely.
var (
	sharedRoutes = buildRoutes()
	sharedRouter = newRouter(sharedRoutes)
)

func newHTTPHandler(c *engine.Cluster) *httpHandler {
	return &httpHandler{c: c, router: sharedRouter}
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
		return h.c.ClusterSettings(params(r))
	})
	add("PUT", "/_cluster/settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutClusterSettings(m, params(r))
	})
	add("GET", "/_cluster/state", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClusterState("", "", params(r), h.address())
	})
	add("GET", "/_cluster/state/{metric}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClusterState(v["metric"], "", params(r), h.address())
	})
	add("GET", "/_cluster/state/{metric}/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClusterState(v["metric"], v["index"], params(r), h.address())
	})
	add("GET", "/_cluster/stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClusterStats(params(r))
	})
	add("GET", "/_nodes", nodesInfo)
	add("GET", "/_nodes/{a}", nodesInfo)
	add("GET", "/_nodes/{a}/{b}", nodesInfo)
	add("GET", "/_nodes/{a}/{b}/{c}", nodesInfo)
	add("GET", "/_cat/indices", catIndices)
	add("GET", "/_cat/indices/{index}", catIndices)
	add("GET", "/_cat/aliases", catAliases)
	add("GET", "/_cat/aliases/{alias}", catAliases)
	add("GET", "/_cat/health", catHealth)
	add("GET", "/_cat/count", catCount)
	add("GET", "/_cat/count/{index}", catCount)
	add("GET", "/_cat/nodes", catNodes)
	add("GET", "/_cat/master", catMaster)
	add("GET", "/_cat/cluster_manager", catMaster)
	add("GET", "/_cat/plugins", catEmpty(catPluginsColumns))
	add("GET", "/_cat/templates", catTemplates)
	add("GET", "/_cat/templates/{name}", catTemplates)
	add("GET", "/_cat/shards", catShards)
	add("GET", "/_cat/shards/{index}", catShards)
	add("GET", "/_cat/segments", catSegments)
	add("GET", "/_cat/segments/{index}", catSegments)
	add("GET", "/_cat/recovery", catRecovery)
	add("GET", "/_cat/recovery/{index}", catRecovery)
	add("GET", "/_cat/allocation", catAllocation)
	add("GET", "/_cat/allocation/{nodes}", catAllocation)
	add("GET", "/_cat/thread_pool", catThreadPool)
	add("GET", "/_cat/thread_pool/{thread_pool_patterns}", catThreadPool)
	add("GET", "/_cat/pending_tasks", catEmpty(catPendingTasksColumns))
	add("GET", "/_cat/fielddata", catEmpty(catFielddataColumns))
	add("GET", "/_cat/fielddata/{fields}", catEmpty(catFielddataColumns))
	add("GET", "/_cat/nodeattrs", catNodeattrs)
	add("GET", "/_cat/tasks", catTasks)
	add("GET", "/_cat/repositories", catEmpty(catRepositoriesColumns))
	add("GET", "/_cat/snapshots", catSnapshots)
	add("GET", "/_cat/snapshots/{repository}", catSnapshots)
	add("GET", "/_cat/segment_replication", catSegmentReplication)
	add("GET", "/_cat/segment_replication/{index}", catSegmentReplication)
	add("GET", "/_cat", catHelpListing)
	// aliases
	add("POST", "/_aliases", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
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
	add("POST", "/_index_template/_simulate", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.SimulateIndexTemplate("", m, params(r))
	})
	add("POST", "/_index_template/_simulate/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.SimulateIndexTemplate(v["name"], m, params(r))
	})
	add("POST", "/_index_template/_simulate_index/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.SimulateIndexTemplateForIndex(v["index"], params(r))
	})
	add("PUT,POST", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutIndexTemplate(v["name"], m, params(r))
	})
	add("GET", "/_index_template", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndexTemplate("", params(r))
	})
	add("GET", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetIndexTemplate(v["name"], params(r))
	})
	add("HEAD", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexTemplateExists(v["name"])
	})
	add("DELETE", "/_index_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteIndexTemplate(v["name"])
	})
	add("PUT,POST", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutLegacyTemplate(v["name"], m, params(r))
	})
	add("GET", "/_template", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetLegacyTemplate("", params(r))
	})
	add("GET", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetLegacyTemplate(v["name"], params(r))
	})
	add("HEAD", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.LegacyTemplateExists(v["name"])
	})
	add("DELETE", "/_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteLegacyTemplate(v["name"])
	})
	// component templates and data streams (route registration added by the
	// admin compatibility work; osmem rejects creating data streams)
	add("PUT,POST", "/_component_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutComponentTemplate(v["name"], m, params(r))
	})
	add("GET", "/_component_template", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetComponentTemplate("", params(r))
	})
	add("GET", "/_component_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetComponentTemplate(v["name"], params(r))
	})
	add("HEAD", "/_component_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ComponentTemplateExists(v["name"])
	})
	add("DELETE", "/_component_template/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteComponentTemplate(v["name"])
	})
	add("GET", "/_data_stream", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetDataStream("")
	})
	add("GET", "/_data_stream/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DataStreamStats("")
	})
	add("GET", "/_data_stream/{name}/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DataStreamStats(v["name"])
	})
	add("PUT", "/_data_stream/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.CreateDataStream(v["name"])
	})
	add("GET", "/_data_stream/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetDataStream(v["name"])
	})
	add("DELETE", "/_data_stream/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DeleteDataStream(v["name"])
	})
	// documents and search, cluster wide
	add("POST,PUT", "/_bulk", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Bulk("", body, params(r))
	})
	add("GET,POST", "/_search", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.search("", r, body)
	})
	add("GET,POST", "/_count", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Count("", m, params(r))
	})
	add("GET,POST", "/_field_caps", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		p := params(r)
		fields := getRequestFields(m, p)
		return h.c.FieldCaps("", fields, p.Bool("include_unmapped", false), m, p)
	})
	add("GET,POST", "/_search_shards", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.SearchShards("", m, params(r), h.address())
	})
	add("GET,POST", "/_msearch", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.MultiSearch("", body, params(r))
	})
	add("GET,POST", "/_mget", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiGet("", m, params(r))
	})
	add("GET,POST", "/_mtermvectors", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiTermVectors("", m, params(r))
	})
	add("GET,POST", "/_search/scroll", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Scroll(m, params(r))
	})
	add("GET,POST", "/_search/scroll/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		m["scroll_id"] = v["id"]
		return h.c.Scroll(m, params(r))
	})
	add("DELETE", "/_search/scroll", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.ClearScroll(m, "")
	})
	add("DELETE", "/_search/scroll/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ClearScroll(nil, v["id"])
	})
	add("DELETE", "/_search/point_in_time", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
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
	refresh := func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Refresh(v["index"], params(r))
	}
	add("POST", "/_refresh", refresh)
	add("GET", "/_refresh", refresh)
	add("POST,GET", "/_flush", ack)
	add("POST,GET", "/_flush/synced", ack)
	add("POST", "/_forcemerge", ack)
	add("POST", "/_cache/clear", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Acknowledge(r.URL.Query().Get("index"), params(r))
	})
	add("POST", "/_reindex", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Reindex(m, params(r))
	})
	add("GET,POST", "/_analyze", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Analyze("", m, params(r), body)
	})
	add("GET", "/_mapping", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetMapping("", params(r))
	})
	add("GET", "/_mappings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetMapping("", params(r))
	})
	add("GET", "/_settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetSettings("", params(r))
	})
	add("GET", "/_settings/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		p := params(r)
		p["settings_filter"] = v["name"]
		return h.c.GetSettings("", p)
	})
	add("GET", "/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexStats("", params(r))
	})
	add("GET", "/_stats/{metric}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		p := params(r)
		p["metric"] = v["metric"]
		return h.c.IndexStats("", p)
	})
	add("GET", "/_resolve/index/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ResolveIndex(v["name"], params(r))
	})
	add("GET,POST", "/_validate/query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ValidateQueryJSON("", body, params(r))
	})
	// index level
	add("PUT", "/{index}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.CreateIndexWithParams(v["index"], m, params(r))
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
	add("GET", "/{index}/_mappings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetMapping(v["index"], params(r))
	})
	add("PUT,POST", "/{index}/_mapping", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
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
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutSettings(v["index"], m, params(r))
	})
	add("PUT", "/_settings", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutSettings("", m, params(r))
	})
	add("GET", "/{index}/_stats", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.IndexStats(v["index"], params(r))
	})
	add("GET", "/{index}/_stats/{metric}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		p := params(r)
		p["metric"] = v["metric"]
		return h.c.IndexStats(v["index"], p)
	})
	add("POST,GET", "/{index}/_refresh", refresh)
	add("POST,GET", "/{index}/_flush", ack)
	add("POST,GET", "/{index}/_flush/synced", ack)
	add("POST", "/{index}/_forcemerge", ack)
	add("POST", "/{index}/_cache/clear", ack)
	add("POST", "/{index}/_open", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.OpenIndex(v["index"], params(r))
	})
	add("POST", "/{index}/_close", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.CloseIndex(v["index"], params(r))
	})
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
		if _, err := decodeBody(r, body); err != nil {
			return engine.Response{}, err
		}
		// the raw body keeps the order of the fields and the locations of
		// parse errors
		return h.c.UpdateDocRaw(v["index"], v["id"], body, params(r))
	})
	add("GET", "/{index}/_source/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetSource(v["index"], v["id"], params(r))
	})
	add("HEAD", "/{index}/_source/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.DocExists(v["index"], v["id"], params(r))
	})
	add("GET,POST", "/{index}/_search", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.search(v["index"], r, body)
	})
	add("GET,POST", "/{index}/_explain/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Explain(v["index"], v["id"], body, params(r))
	})
	add("GET,POST", "/{index}/_validate/query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.ValidateQueryJSON(v["index"], body, params(r))
	})
	add("POST", "/{index}/_search/point_in_time", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.CreatePIT(v["index"], params(r))
	})
	add("GET,POST", "/{index}/_count", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Count(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_search_shards", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.SearchShards(v["index"], m, params(r), h.address())
	})
	add("GET,POST", "/{index}/_field_caps", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		p := params(r)
		fields := getRequestFields(m, p)
		return h.c.FieldCaps(v["index"], fields, p.Bool("include_unmapped", false), m, p)
	})
	add("GET,POST", "/{index}/_msearch", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.MultiSearch(v["index"], body, params(r))
	})
	add("GET,POST", "/{index}/_mget", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiGet(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_mtermvectors", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.MultiTermVectors(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_termvectors/{id}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.TermVectors(v["index"], v["id"], m, params(r))
	})
	add("POST,PUT", "/{index}/_bulk", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.Bulk(v["index"], body, params(r))
	})
	add("POST", "/{index}/_delete_by_query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.DeleteByQuery(v["index"], m, params(r))
	})
	add("POST", "/{index}/_update_by_query", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.UpdateByQuery(v["index"], m, params(r))
	})
	add("GET,POST", "/{index}/_analyze", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.Analyze(v["index"], m, params(r), body)
	})
	// alias routes (RestIndexPutAliasAction, RestIndexDeleteAliasesAction,
	// RestGetAliasesAction): the put variants without an index or alias in
	// the path take them from the body; get/head only exist for _alias.
	putAlias := func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		return h.c.PutAlias(v["index"], v["name"], m)
	}
	for _, seg := range []string{"_alias", "_aliases"} {
		add("PUT,POST", "/{index}/"+seg+"/{name}", putAlias)
		add("PUT", "/{index}/"+seg, putAlias)
		add("PUT,POST", "/"+seg+"/{name}", putAlias)
		add("DELETE", "/{index}/"+seg+"/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
			return h.c.DeleteAlias(v["index"], v["name"])
		})
	}
	add("PUT", "/_alias", putAlias)
	add("GET", "/{index}/_alias/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetAliases(v["index"], v["name"], params(r))
	})
	add("HEAD", "/{index}/_alias/{name}", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.AliasExists(v["index"], v["name"], params(r))
	})
	add("GET", "/{index}/_alias", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.GetAliases(v["index"], "", params(r))
	})
	add("HEAD", "/{index}/_alias", func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		return h.c.AliasExists(v["index"], "", params(r))
	})
	return routes
}

// rawText marks a plain-text response body.
type rawText string

type paramsKey struct{}

// withParams attaches the decoded query string to the request so that the
// handler, the response writer and the error path share one parse.
func withParams(r *http.Request, p engine.Params) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), paramsKey{}, p))
}

// params returns the URL parameters of a request with OpenSearch's query
// string decoding (a repeated parameter keeps its last value). Requests with
// undecodable parameters are rejected before handlers run. ServeHTTP parses
// the query string once and attaches it to the request; the fallback parse
// serves requests built elsewhere (tests, the admin API).
func params(r *http.Request) engine.Params {
	if p, ok := r.Context().Value(paramsKey{}).(engine.Params); ok {
		return p
	}
	p, err := parseQueryString(r.URL.RawQuery)
	if err != nil {
		return engine.Params{}
	}
	return p
}

func docParams(r *http.Request) (engine.DocParams, error) {
	return engine.DocParamsFrom(params(r))
}

// search runs a _search the way RestSearchAction parses it: content after
// the main object fails once the object parsed cleanly.
func (h *httpHandler) search(index string, r *http.Request, body []byte) (engine.Response, error) {
	m, trailing, err := engine.DecodeSearchBodyParts(body)
	if err != nil {
		return engine.Response{}, err
	}
	traceBody(r, body, m)
	res, err := h.c.SearchSource(index, body, m, params(r))
	if trailing != nil {
		if e, ok := err.(*engine.Error); ok && (parseErrorTypes[e.Type] || engine.IsParseFailure(e)) {
			return res, err
		}
		return engine.Response{}, trailing
	}
	return res, err
}

func getRequestFields(body M, p engine.Params) []string {
	switch fields := body["fields"].(type) {
	case string:
		if fields != "" {
			return strings.Split(fields, ",")
		}
	case []any:
		out := make([]string, 0, len(fields))
		for _, field := range fields {
			if value, ok := field.(string); ok {
				out = append(out, value)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if fieldParam := p.Get("fields"); fieldParam != "" {
		return strings.Split(fieldParam, ",")
	}
	return nil
}

// parseErrorTypes are failures OpenSearch raises while parsing a search
// body, before it reads the URL parameters.
var parseErrorTypes = map[string]bool{"parsing_exception": true, "x_content_parse_exception": true,
	"json_parse_exception": true, "named_object_not_found_exception": true}

// maxBodyBytes is OpenSearch's default http.max_content_length (100mb); a
// larger body, before or after gzip decoding, is refused with 413 instead
// of being read into memory.
const maxBodyBytes = 100 << 20

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			writeError(w, r, &engine.Error{Status: 400, Type: "parsing_exception", Reason: "invalid gzip body: " + err.Error()})
			return
		}
		defer gz.Close()
		reader = io.LimitReader(gz, maxBodyBytes+1)
	}
	body, err := io.ReadAll(reader)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || len(body) > maxBodyBytes {
		w.Header().Set("Connection", "close")
		writeResponseWith(w, r, responseOptions{}, engine.Response{Status: http.StatusRequestEntityTooLarge,
			Body: M{"error": "request body is larger than " + strconv.Itoa(maxBodyBytes) + " bytes", "status": http.StatusRequestEntityTooLarge}})
		return
	}
	if err != nil {
		writeError(w, r, &engine.Error{Status: 400, Type: "parsing_exception", Reason: err.Error()})
		return
	}
	// OpenSearch rejects undecodable parameters, a malformed Content-Type and
	// invalid pretty/human/error_trace values before routing, answering
	// without applying any parameter.
	p, perr := parseQueryString(r.URL.RawQuery)
	if perr != nil {
		writeResponseWith(w, r, responseOptions{}, engine.Response{Status: perr.Status, Body: perr.Body(false)})
		return
	}
	r = withParams(r, p)
	media, cterr := parseContentType(r.Header.Values("Content-Type"))
	if cterr == nil {
		cterr = validateChannelParams(p)
	}
	if cterr != nil {
		writeResponseWith(w, r, responseOptions{}, engine.Response{Status: cterr.Status, Body: cterr.Body(false)})
		return
	}
	opts := responseOptionsFor(p)
	opts.unfiltered = true
	rawPath := r.URL.EscapedPath()
	if !restMethods[r.Method] {
		msg := "Unexpected HTTP method"
		if allowed := h.router.validMethods(rawPath); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ","))
			msg += ", allowed: [" + strings.Join(allowed, ", ") + "]"
		}
		writeResponseWith(w, r, opts, engine.Response{Status: http.StatusMethodNotAllowed, Body: M{"error": msg, "status": http.StatusMethodNotAllowed}})
		return
	}
	rt, _, vars, allowed, outcome := h.router.dispatch(r.Method, rawPath)
	switch outcome {
	case dispatchOptions:
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ","))
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		return
	case dispatchMethodNotAllowed:
		w.Header().Set("Allow", strings.Join(allowed, ","))
		msg := fmt.Sprintf("Incorrect HTTP method for uri [%s] and method [%s], allowed: [%s]", r.URL.RequestURI(), r.Method, strings.Join(allowed, ", "))
		writeResponseWith(w, r, opts, engine.Response{Status: http.StatusMethodNotAllowed, Body: M{"error": msg, "status": http.StatusMethodNotAllowed}})
		return
	case dispatchNoHandler:
		writeResponseWith(w, r, opts, engine.Response{Status: http.StatusBadRequest, Body: M{"error": fmt.Sprintf("no handler found for uri [%s] and method [%s]", r.URL.Path, r.Method)}})
		return
	}
	if len(body) > 0 && media != mediaJSON {
		// osmem parses JSON bodies only; OpenSearch also reads SMILE, YAML
		// and CBOR but rejects other media types this way.
		header := strings.Join(r.Header.Values("Content-Type"), ",")
		writeResponseWith(w, r, opts, engine.Response{Status: http.StatusNotAcceptable,
			Body: M{"error": "Content-Type header [" + header + "] is not supported", "status": http.StatusNotAcceptable}})
		return
	}
	api := apiFor(r.Method, rt)
	if api != nil {
		ctx := &requestContext{params: p, body: body, path: r.URL.Path, vars: vars}
		if api.source && len(body) == 0 && p.Has("source") {
			src, serr := sourceParamBody(p)
			if serr != nil {
				writeError(w, r, serr)
				return
			}
			body, ctx.body, ctx.sourceUsed = src, src, true
		}
		if api.bodyFirst {
			if berr := api.checkBody(body); berr != nil {
				writeError(w, r, berr)
				return
			}
		}
		if verr := api.validate(ctx); verr != nil {
			if api.bodyFirst && len(body) > 0 {
				// the body is parsed before the parameters: report its
				// failure first
				trial := withParams(r.Clone(r.Context()), engine.Params{})
				trial.URL.RawQuery = ""
				if _, ferr := h.run(rt, trial, vars, body); ferr != nil {
					if e, ok := ferr.(*engine.Error); ok && (parseErrorTypes[e.Type] || engine.IsParseFailure(e)) {
						writeError(w, r, e)
						return
					}
				}
			}
			writeError(w, r, verr)
			return
		}
		if !api.bodyFirst {
			if berr := api.checkBody(body); berr != nil {
				writeError(w, r, berr)
				return
			}
		}
	}
	if api != nil {
		if query, changed := api.engineQuery(p); changed {
			// hand the engine the parameters as OpenSearch interprets them
			r = r.Clone(r.Context())
			r.URL.RawQuery = query
			if ep, eerr := parseQueryString(query); eerr == nil {
				r = withParams(r, ep)
			}
		}
	}
	res, err := h.run(rt, r, vars, body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if api != nil && api.render != nil && res.Status < 300 {
		if rerr := api.render(&requestContext{params: p, path: r.URL.Path, vars: vars}); rerr != nil {
			writeError(w, r, rerr)
			return
		}
	}
	opts.unfiltered = false
	writeResponseWith(w, r, opts, forcedRefresh(p, r, res))
}

// sourceParamBody is RestRequest.contentOrSourceParam for a request without
// a body.
func sourceParamBody(p engine.Params) ([]byte, *engine.Error) {
	source, hasSource := p["source"]
	contentType, hasType := p["source_content_type"]
	if !hasSource || !hasType {
		return nil, &engine.Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: "source and source_content_type parameters are required"}
	}
	kind, err := parseContentType([]string{contentType})
	if err != nil {
		return nil, illegalArgument(err.Cause.Reason)
	}
	if kind != mediaJSON {
		return nil, &engine.Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: "Unknown value for source_content_type [" + contentType + "]"}
	}
	if source == "" {
		// an empty source is content without a token
		return []byte(" "), nil
	}
	return []byte(source), nil
}

// checkBody reports a body the handler's parser rejects before reading it.
func (api *restAPI) checkBody(body []byte) error {
	if api.body == bodySearch {
		if _, _, err := engine.DecodeSearchBodyParts(body); err != nil {
			return err
		}
		return nil
	}
	if err := api.bodyShapeError(body); err != nil {
		return err
	}
	if engine.FirstJSONToken(body, 0).Name != "START_OBJECT" {
		return nil
	}
	switch api.body {
	case bodyScroll:
		if _, err := engine.DecodeObject(body); err != nil {
			if e, ok := err.(*engine.Error); ok && e.Type == "json_parse_exception" {
				return &engine.Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse request body", Cause: e}
			}
		}
	case bodyMap:
		// XContentHelper.convertToMap wraps every parse failure
		if _, err := engine.DecodeObject(body); err != nil {
			if e, ok := err.(*engine.Error); ok && e.Type == "json_parse_exception" {
				return &engine.Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "Failed to parse content to map", Cause: e}
			}
		}
	case bodyQuery:
		// RestActions.getQueryContent reports where parsing stopped
		if _, err := engine.DecodeObject(body); err != nil {
			if dup := engine.FindDuplicateKey(body); dup != nil {
				line, col := engine.TokenLocationBefore(body, dup.Offset-len(dup.Name)-2)
				e, _ := err.(*engine.Error)
				return &engine.Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Failed to parse",
					Extra: map[string]any{"line": line, "col": col}, Cause: e}
			}
		}
	}
	return nil
}

// forcedRefresh marks write responses the way OpenSearch does for
// refresh=true: the document, or each successful bulk item, reports
// forced_refresh.
func forcedRefresh(p engine.Params, r *http.Request, res engine.Response) engine.Response {
	v, ok := p["refresh"]
	if !ok || (v != "" && v != "true") || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return res
	}
	body, isMap := res.Body.(M)
	if !isMap {
		return res
	}
	if items, ok := body["items"].([]any); ok {
		for _, it := range items {
			item, _ := it.(M)
			for _, inner := range item {
				if m, ok := inner.(M); ok && m["error"] == nil {
					m["forced_refresh"] = true
				}
			}
		}
		return res
	}
	if _, ok := body["result"]; ok {
		body["forced_refresh"] = true
	}
	return res
}

// responseOptions are the response formatting parameters.
type responseOptions struct {
	pretty     bool
	errorTrace bool // fabricate a stack_trace on every error rendered with these options
	filter     filterPath
	filtered   bool
	filterErr  *engine.Error
	unfiltered bool // error responses are never filtered
}

func responseOptionsFor(p engine.Params) responseOptions {
	var opts responseOptions
	if v, ok := p["pretty"]; ok {
		opts.pretty, _ = engine.ParseBoolValue(v, false)
	}
	if v, ok := p["error_trace"]; ok {
		opts.errorTrace, _ = engine.ParseBoolValue(v, false)
	}
	opts.filter, opts.filtered, opts.filterErr = parseFilterPath(p.Get("filter_path"))
	return opts
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := err.(*engine.Error)
	if !ok {
		e = &engine.Error{Status: 500, Type: "exception", Reason: err.Error()}
	}
	opts := responseOptionsFor(params(r))
	opts.unfiltered = true
	writeResponseWith(w, r, opts, engine.Response{Status: e.Status, Body: e.Body(opts.errorTrace)})
}

func writeResponse(w http.ResponseWriter, r *http.Request, res engine.Response) {
	writeResponseWith(w, r, responseOptionsFor(params(r)), res)
}

func writeResponseWith(w http.ResponseWriter, r *http.Request, opts responseOptions, res engine.Response) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	if r.Method == "HEAD" || res.Body == nil {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
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
	if opts.filtered && !opts.unfiltered {
		if opts.filterErr != nil {
			e := opts.filterErr
			opts.unfiltered = true
			writeResponseWith(w, r, opts, engine.Response{Status: e.Status, Body: e.Body(opts.errorTrace)})
			return
		}
		filtered, keep := opts.filter.apply(body)
		if !keep {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(res.Status)
			return
		}
		body = filtered
	}
	data, err := engine.EncodeJSON(body, opts.pretty)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(500)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"exception","reason":%q},"status":500}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(res.Status)
	_, _ = w.Write(data)
}

// cat APIs ---------------------------------------------------------------
//
// The cat handlers follow OpenSearch's RestTable: each API declares its
// columns with the attribute strings of Table.addCell (aliases, description,
// default visibility, alignment, sibling) and builds typed cells; catRender
// then applies help, h, v, s, ts, pri, bytes, time and format (or the Accept
// header) the way RestTable and AbstractCatAction do.

// Cell types that render like their Java counterparts: catBytes is a
// ByteSizeValue (bytes), catTime a TimeValue (nanoseconds) and catDouble a
// Double. int is a Java Integer and int64 a Long (they differ when sorting
// mixed columns).
type (
	catBytes  int64
	catTime   int64
	catDouble float64
)

type catColumn struct {
	name, alias, desc, sibling string
	hidden, right              bool
}

// catColumns parses Table.addCell specs such as
// "pri;alias:p,shards.primary;text-align:right;desc:number of primary shards".
func catColumns(specs ...string) []catColumn {
	cols := make([]catColumn, 0, len(specs))
	for _, spec := range specs {
		parts := strings.Split(spec, ";")
		col := catColumn{name: parts[0]}
		for _, attr := range parts[1:] {
			k, v, _ := strings.Cut(attr, ":")
			switch k {
			case "alias":
				col.alias = v
			case "desc":
				col.desc = v
			case "default":
				col.hidden = v == "false"
			case "text-align":
				col.right = v == "right"
			case "sibling":
				col.sibling = v
			}
		}
		cols = append(cols, col)
	}
	return cols
}

type catTable struct {
	cols []catColumn
	rows []map[string]any
}

func (t *catTable) column(name string) (catColumn, bool) {
	for _, c := range t.cols {
		if c.name == name {
			return c, true
		}
	}
	return catColumn{}, false
}

// catRequest carries what every cat handler needs from the request.
type catRequest struct {
	r    *http.Request
	q    engine.Params
	vars map[string]string
}

func newCatRequest(r *http.Request, vars map[string]string) *catRequest {
	return &catRequest{r: r, q: params(r), vars: vars}
}

// param reads a request parameter; path parameters win over the query
// string like in RestRequest (/_cat/indices?index=x works too).
func (cr *catRequest) param(name string) (string, bool) {
	if v, ok := cr.vars[name]; ok {
		return v, true
	}
	if v, ok := cr.q[name]; ok {
		return v, true
	}
	return "", false
}

func (cr *catRequest) get(name string) string {
	v, _ := cr.param(name)
	return v
}

// boolean implements RestRequest.paramAsBoolean: a bare flag is true and
// anything but true/false is rejected.
func (cr *catRequest) boolean(name string, def bool) (bool, error) {
	v, ok := cr.param(name)
	if !ok {
		return def, nil
	}
	switch v {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, &engine.Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse value [" + v + "] as only [true] or [false] are allowed."}
}

// engineParams returns the request parameters for engine calls, including
// path parameters.
func (cr *catRequest) engineParams() engine.Params {
	p := make(engine.Params, len(cr.q)+len(cr.vars))
	for k, v := range cr.q {
		p[k] = v
	}
	for k, v := range cr.vars {
		p[k] = v
	}
	return p
}

// catHelpResponseParams are AbstractCatAction.RESPONSE_PARAMS plus the
// parameters consumed before a handler runs (help, format, filter_path,
// pretty, human, error_trace).
var catHelpResponseParams = []string{"format", "h", "v", "ts", "pri", "bytes", "size", "time", "s", "timeout", "help", "filter_path", "pretty", "human", "error_trace"}

// catHelp answers ?help. Like OpenSearch, a help request consumes no other
// parameter, so path parameters (and anything that is not a response
// parameter of the action) are reported as unrecognized.
func catHelp(cr *catRequest, cols []catColumn, extraResponseParams ...string) (engine.Response, bool, error) {
	help, err := cr.boolean("help", false)
	if err != nil || !help {
		return engine.Response{}, err != nil, err
	}
	allowed := map[string]bool{}
	for _, p := range append(append([]string{}, catHelpResponseParams...), extraResponseParams...) {
		allowed[p] = true
	}
	var unknown []string
	seen := map[string]bool{}
	for k := range cr.vars {
		if !allowed[k] && !seen[k] {
			seen[k] = true
			unknown = append(unknown, k)
		}
	}
	for k := range cr.q {
		if !allowed[k] && !seen[k] {
			seen[k] = true
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		candidates := make([]string, 0, len(allowed))
		for k := range allowed {
			candidates = append(candidates, k)
		}
		return engine.Response{}, true, engine.UnrecognizedError(cr.r.URL.Path, unknown, candidates, "parameter")
	}
	var widths [3]int
	cells := make([][3]string, len(cols))
	for i, c := range cols {
		desc := c.desc
		if desc == "" {
			desc = "not available"
		}
		cells[i] = [3]string{c.name, c.alias, desc}
		for j, s := range cells[i] {
			widths[j] = max(widths[j], catLen(s))
		}
	}
	var sb strings.Builder
	for _, row := range cells {
		for j, s := range row {
			if j > 0 {
				sb.WriteString(" | ")
			}
			catPad(&sb, s, false, widths[j], false, false)
		}
		sb.WriteByte('\n')
	}
	return engine.Response{Status: http.StatusOK, Body: rawText(sb.String())}, true, nil
}

// catDisplay is RestTable.DisplayHeader: the column and the name shown.
type catDisplay struct {
	name, display string
	right         bool
}

type catFormat int

const (
	catFormatText catFormat = iota
	catFormatJSON
	catFormatYAML
	catFormatCBOR
	catFormatSmile
)

// catMediaType picks the output format from ?format (unknown values mean
// text) or, without it, from the Accept header.
func catMediaType(cr *catRequest) catFormat {
	if v, ok := cr.q["format"]; ok {
		switch strings.ToLower(v) {
		case "json":
			return catFormatJSON
		case "yaml":
			return catFormatYAML
		case "cbor":
			return catFormatCBOR
		case "smile":
			return catFormatSmile
		}
		return catFormatText
	}
	accept := strings.ToLower(strings.TrimSpace(cr.r.Header.Get("Accept")))
	if i := strings.IndexByte(accept, ';'); i >= 0 {
		accept = strings.TrimSpace(accept[:i])
	}
	switch accept {
	case "application/json", "application/x-ndjson", "application/*", "application/vnd.opensearch+json":
		return catFormatJSON
	case "application/yaml":
		return catFormatYAML
	case "application/cbor":
		return catFormatCBOR
	case "application/smile":
		return catFormatSmile
	}
	return catFormatText
}

// catRender builds the response of a cat table (RestTable.buildResponse).
func catRender(cr *catRequest, t *catTable) (engine.Response, error) {
	format := catMediaType(cr)
	verbose := false
	if format == catFormatText {
		var err error
		if verbose, err = cr.boolean("v", false); err != nil {
			return engine.Response{}, err
		}
	}
	headers, err := catDisplayHeaders(cr, t)
	if err != nil {
		return engine.Response{}, err
	}
	order, err := catRowOrder(cr, t)
	if err != nil {
		return engine.Response{}, err
	}
	cells := make([][]*string, len(order))
	for i, ri := range order {
		cells[i] = make([]*string, len(headers))
		for j, hd := range headers {
			cells[i][j] = catRenderValue(cr, t.rows[ri][hd.name])
		}
	}
	switch format {
	case catFormatJSON:
		v, pretty := cr.q["pretty"]
		pretty = pretty && v != "false"
		return engine.Response{Status: http.StatusOK, Body: catJSON(headers, cells, pretty)}, nil
	case catFormatYAML:
		return engine.Response{Status: http.StatusOK, Body: rawText(catYAML(headers, cells))}, nil
	case catFormatCBOR:
		return engine.Response{Status: http.StatusOK, Body: rawText(catCBOR(headers, cells))}, nil
	case catFormatSmile:
		return engine.Response{Status: http.StatusOK, Body: rawText(catSmile(headers, cells))}, nil
	}
	return engine.Response{Status: http.StatusOK, Body: rawText(catText(headers, cells, verbose))}, nil
}

// catDisplayHeaders implements RestTable.buildDisplayHeaders.
func catDisplayHeaders(cr *catRequest, t *catTable) ([]catDisplay, error) {
	var out []catDisplay
	h, ok := cr.q["h"]
	if !ok {
		for _, c := range t.cols {
			if c.hidden {
				continue
			}
			show, err := catShowTimestamp(cr, c.name)
			if err != nil {
				return nil, err
			}
			if show {
				out = append(out, catDisplay{c.name, c.name, c.right})
			}
		}
		return out, nil
	}
	for _, possibility := range catExpandHeaders(t, h) {
		var disp *catDisplay
		if c, ok := t.column(possibility); ok {
			disp = &catDisplay{c.name, possibility, c.right}
		} else {
			// the last column declaring the alias wins
			for _, c := range t.cols {
				for _, a := range catSplitComma(c.alias) {
					if a == possibility {
						disp = &catDisplay{c.name, a, c.right}
						break
					}
				}
			}
		}
		if disp == nil {
			continue
		}
		show, err := catShowTimestamp(cr, disp.name)
		if err != nil {
			return nil, err
		}
		if !show {
			continue
		}
		out = append(out, *disp)
		if c, _ := t.column(disp.name); c.sibling != "" {
			if sib, ok := t.column(c.sibling + "." + disp.name); ok {
				on, err := cr.boolean(c.sibling, false)
				if err != nil {
					return nil, err
				}
				if on {
					out = append(out, catDisplay{sib.name, c.sibling + "." + disp.display, sib.right})
				}
			}
		}
	}
	return out, nil
}

func catShowTimestamp(cr *catRequest, name string) (bool, error) {
	if name == "epoch" || name == "timestamp" {
		return cr.boolean("ts", true)
	}
	return true, nil
}

// catExpandHeaders implements RestTable.expandHeadersFromRequest.
func catExpandHeaders(t *catTable, h string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, header := range catSplitComma(h) {
		if !strings.Contains(header, "*") {
			add(header)
			continue
		}
		for _, c := range t.cols {
			if globMatch(header, c.name) {
				add(c.name)
				continue
			}
			for _, a := range catSplitComma(c.alias) {
				if globMatch(header, a) {
					add(c.name)
					break
				}
			}
		}
	}
	return out
}

// catRowOrder implements RestTable.getRowOrder (?s=col[:asc|:desc],...).
func catRowOrder(cr *catRequest, t *catTable) ([]int, error) {
	order := make([]int, len(t.rows))
	for i := range order {
		order[i] = i
	}
	s, ok := cr.q["s"]
	if !ok {
		return order, nil
	}
	aliases := map[string]string{}
	for _, c := range t.cols {
		for _, a := range catSplitComma(c.alias) {
			aliases[a] = c.name
		}
		aliases[c.name] = c.name
	}
	type sortKey struct {
		column  string
		reverse bool
	}
	var keys []sortKey
	for _, key := range catSplitComma(s) {
		reverse := false
		if strings.HasSuffix(key, ":desc") {
			key, reverse = strings.TrimSuffix(key, ":desc"), true
		} else if strings.HasSuffix(key, ":asc") {
			key = strings.TrimSuffix(key, ":asc")
		}
		column, ok := aliases[key]
		if !ok {
			return nil, &engine.Error{Status: http.StatusInternalServerError, Type: "unsupported_operation_exception", Reason: "Unable to sort by unknown sort key `" + key + "`"}
		}
		keys = append(keys, sortKey{column, reverse})
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := t.rows[order[i]], t.rows[order[j]]
		for _, k := range keys {
			if c := catCompare(a[k.column], b[k.column]); c != 0 {
				if k.reverse {
					return c > 0
				}
				return c < 0
			}
		}
		return false
	})
	return order, nil
}

// catCompare is RestTable.TableIndexComparator.compareCell: nulls first,
// same-typed values by their natural order, anything else as strings.
func catCompare(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	switch x := a.(type) {
	case int:
		if y, ok := b.(int); ok {
			return cmp.Compare(x, y)
		}
	case int64:
		if y, ok := b.(int64); ok {
			return cmp.Compare(x, y)
		}
	case catBytes:
		if y, ok := b.(catBytes); ok {
			return cmp.Compare(x, y)
		}
	case catTime:
		if y, ok := b.(catTime); ok {
			return cmp.Compare(x, y)
		}
	case catDouble:
		if y, ok := b.(catDouble); ok {
			return cmp.Compare(x, y)
		}
	case bool:
		if y, ok := b.(bool); ok {
			switch {
			case x == y:
				return 0
			case !x:
				return -1
			}
			return 1
		}
	}
	return catCompareStrings(catString(a), catString(b))
}

// catCompareStrings compares like Java's String.compareTo (UTF-16 units).
func catCompareStrings(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return int(ua[i]) - int(ub[i])
		}
	}
	return len(ua) - len(ub)
}

// catString is the Java toString of a cell.
func catString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	case catBytes:
		return catBytesString(int64(x))
	case catTime:
		return catTimeString(int64(x))
	case catDouble:
		return catJavaDouble(float64(x))
	}
	return fmt.Sprint(v)
}

// catRenderValue is RestTable.renderValue; nil means a null cell.
func catRenderValue(cr *catRequest, v any) *string {
	if v == nil {
		return nil
	}
	s := catString(v)
	switch x := v.(type) {
	case catBytes:
		if unit, ok := catByteUnits[cr.q.Get("bytes")]; ok {
			s = strconv.FormatInt(int64(x)/unit, 10)
		}
	case catTime:
		if unit, ok := catTimeUnits[cr.q.Get("time")]; ok {
			s = strconv.FormatInt(int64(x)/unit, 10)
		}
	}
	return &s
}

var (
	catByteUnits = map[string]int64{"b": 1, "kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40, "pb": 1 << 50}
	catTimeUnits = map[string]int64{"nanos": 1, "micros": 1e3, "ms": 1e6, "s": 1e9, "m": 60e9, "h": 3600e9, "d": 86400e9}
)

// catBytesString is ByteSizeValue.toString.
func catBytesString(b int64) string {
	value, suffix := float64(b), "b"
	for _, u := range []struct {
		size   int64
		suffix string
	}{{1 << 50, "pb"}, {1 << 40, "tb"}, {1 << 30, "gb"}, {1 << 20, "mb"}, {1 << 10, "kb"}} {
		if b >= u.size {
			value, suffix = float64(b)/float64(u.size), u.suffix
			break
		}
	}
	// Strings.format1Decimals keeps the first fraction digit unless it is 0
	p := catJavaDouble(value)
	intPart, frac, _ := strings.Cut(p, ".")
	if frac == "" || frac[0] == '0' {
		return intPart + suffix
	}
	return intPart + "." + frac[:1] + suffix
}

// catTimeString is TimeValue.toString (toHumanReadableString(1)).
func catTimeString(nanos int64) string {
	if nanos < 0 {
		return strconv.FormatInt(nanos, 10)
	}
	if nanos == 0 {
		return "0s"
	}
	value, suffix := float64(nanos), "nanos"
	for _, u := range []struct {
		size   int64
		suffix string
	}{{86400e9, "d"}, {3600e9, "h"}, {60e9, "m"}, {1e9, "s"}, {1e6, "ms"}, {1e3, "micros"}} {
		if nanos >= u.size {
			value, suffix = float64(nanos)/float64(u.size), u.suffix
			break
		}
	}
	p := catJavaDouble(value)
	intPart, frac, _ := strings.Cut(p, ".")
	if frac == "" || frac[0] == '0' {
		return intPart + suffix
	}
	return intPart + "." + frac[:1] + suffix
}

// catJavaDouble renders a double like Double.toString for the magnitudes
// the cat tables use (plain notation between 1e-3 and 1e7).
func catJavaDouble(v float64) string {
	if v != 0 && (math.Abs(v) < 1e-3 || math.Abs(v) >= 1e7) {
		s := strconv.FormatFloat(v, 'E', -1, 64)
		mant, exp, _ := strings.Cut(s, "E")
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + "E" + strings.TrimPrefix(strings.TrimLeft(exp, "+"), "0")
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// catJavaPercent formats like String.format("%1.1f%%", v): HALF_UP on the
// shortest decimal representation.
func catJavaPercent(v float64) string {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', -1, 64))
	if !ok {
		return strconv.FormatFloat(v, 'f', 1, 64) + "%"
	}
	neg := r.Sign() < 0
	r.Abs(r)
	r.Mul(r, big.NewRat(10, 1)).Add(r, big.NewRat(1, 2))
	digits := new(big.Int).Quo(r.Num(), r.Denom()).String()
	if len(digits) < 2 {
		digits = "0" + digits
	}
	s := digits[:len(digits)-1] + "." + digits[len(digits)-1:] + "%"
	if neg {
		s = "-" + s
	}
	return s
}

// catLen is Java's String.length (UTF-16 units).
func catLen(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// catPad is RestTable.pad. The leftover width is a Java byte, so a column
// more than 127 characters wider than the value gets no padding at all.
func catPad(sb *strings.Builder, value string, null bool, width int, right, last bool) {
	length := 0
	if !null {
		length = catLen(value)
	}
	leftOver := int8(width - length)
	if leftOver > 0 && right {
		sb.WriteString(strings.Repeat(" ", int(leftOver)))
		sb.WriteString(value)
		return
	}
	sb.WriteString(value)
	if !last && leftOver > 0 {
		sb.WriteString(strings.Repeat(" ", int(leftOver)))
	}
}

// catText renders the plain text table (RestTable.buildTextPlainResponse).
func catText(headers []catDisplay, cells [][]*string, verbose bool) string {
	widths := make([]int, len(headers))
	if verbose {
		for i, hd := range headers {
			widths[i] = catLen(hd.display)
		}
	}
	for _, row := range cells {
		for i, v := range row {
			if v != nil {
				widths[i] = max(widths[i], catLen(*v))
			}
		}
	}
	var sb strings.Builder
	last := len(headers) - 1
	if verbose {
		for i, hd := range headers {
			catPad(&sb, hd.display, false, widths[i], hd.right, i == last)
			if i != last {
				sb.WriteByte(' ')
			}
		}
		sb.WriteByte('\n')
	}
	for _, row := range cells {
		for i, v := range row {
			if v == nil {
				catPad(&sb, "", true, widths[i], headers[i].right, i == last)
			} else {
				catPad(&sb, *v, false, widths[i], headers[i].right, i == last)
			}
			if i != last {
				sb.WriteByte(' ')
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// catJSON renders the rows as a JSON array keeping the column order (the
// generic encoder sorts object keys), with Jackson's pretty layout.
func catJSON(headers []catDisplay, cells [][]*string, pretty bool) json.RawMessage {
	var b bytes.Buffer
	str := func(s string) {
		data, _ := engine.EncodeJSON(s, false)
		b.Write(data)
	}
	b.WriteByte('[')
	if len(cells) == 0 && pretty {
		b.WriteByte(' ')
	}
	for i, row := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		if pretty {
			b.WriteString("\n  ")
		}
		b.WriteByte('{')
		if len(row) == 0 && pretty {
			b.WriteByte(' ')
		}
		for j, v := range row {
			if j > 0 {
				b.WriteByte(',')
			}
			if pretty {
				b.WriteString("\n    ")
			}
			str(headers[j].display)
			if pretty {
				b.WriteString(" : ")
			} else {
				b.WriteByte(':')
			}
			if v == nil {
				b.WriteString("null")
			} else {
				str(*v)
			}
		}
		if pretty && len(row) > 0 {
			b.WriteString("\n  ")
		}
		b.WriteByte('}')
	}
	if pretty && len(cells) > 0 {
		b.WriteByte('\n')
	}
	b.WriteByte(']')
	return json.RawMessage(b.Bytes())
}

// catYAML renders the rows like Jackson's YAMLGenerator (SnakeYAML block
// style, double-quoted strings split at spaces beyond 80 columns).
func catYAML(headers []catDisplay, cells [][]*string) string {
	var sb strings.Builder
	if len(cells) == 0 {
		return "--- []\n"
	}
	sb.WriteString("---\n")
	for _, row := range cells {
		if len(row) == 0 {
			sb.WriteString("- {}\n")
			continue
		}
		for j, v := range row {
			if j == 0 {
				sb.WriteString("- ")
			} else {
				sb.WriteString("  ")
			}
			sb.WriteString(headers[j].display)
			sb.WriteString(":")
			if v == nil {
				sb.WriteString(" null\n")
				continue
			}
			sb.WriteByte(' ')
			catYAMLQuoted(&sb, *v, 2+catLen(headers[j].display)+2)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

var catYAMLEscapes = map[rune]string{0: "0", '\a': "a", '\b': "b", '\t': "t", '\n': "n", '\v': "v", '\f': "f", '\r': "r", 0x1b: "e", '"': "\"", '\\': "\\", 0x85: "N", 0xa0: "_", 0x2028: "L", 0x2029: "P"}

// catYAMLQuoted is SnakeYAML's Emitter.writeDoubleQuoted with split lines,
// best width 80 and an indent of 4 for continuation lines.
func catYAMLQuoted(sb *strings.Builder, text string, column int) {
	const bestWidth, indent = 80, 4
	chars := utf16.Encode([]rune(text))
	sb.WriteByte('"')
	column++
	write := func(s string) {
		sb.WriteString(s)
		column += catLen(s)
	}
	units := func(from, to int) string { return string(utf16.Decode(chars[from:to])) }
	start, end := 0, 0
	for end <= len(chars) {
		var ch rune = -1
		if end < len(chars) {
			ch = rune(chars[end])
		}
		if ch == -1 || ch == '"' || ch == '\\' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || ch == 0xfeff || ch < 0x20 || ch > 0x7e {
			if start < end {
				write(units(start, end))
				start = end
			}
			if ch != -1 {
				var data string
				if e, ok := catYAMLEscapes[ch]; ok {
					data = "\\" + e
				} else if !catYAMLPrintable(ch) {
					switch {
					case ch <= 0xff:
						data = fmt.Sprintf("\\x%02x", ch)
					case ch >= 0xd800 && ch <= 0xdbff && end+1 < len(chars):
						end++
						data = fmt.Sprintf("\\U%08x", utf16.DecodeRune(ch, rune(chars[end])))
					default:
						data = fmt.Sprintf("\\u%04x", ch)
					}
				} else if ch >= 0xd800 && ch <= 0xdbff && end+1 < len(chars) {
					end++
					data = string(utf16.DecodeRune(ch, rune(chars[end])))
				} else {
					data = string(ch)
				}
				write(data)
				start = end + 1
			}
		}
		if 0 < end && end < len(chars)-1 && (ch == ' ' || start >= end) && column+(end-start) > bestWidth {
			data := "\\"
			if start < end {
				data = units(start, end) + "\\"
				start = end
			}
			write(data)
			sb.WriteByte('\n')
			sb.WriteString(strings.Repeat(" ", indent))
			column = indent
			if chars[start] == ' ' {
				write("\\")
			}
		}
		end++
	}
	sb.WriteByte('"')
}

// catYAMLPrintable is SnakeYAML's StreamReader.isPrintable.
func catYAMLPrintable(c rune) bool {
	return c == 0x9 || c == 0xa || c == 0xd || (c >= 0x20 && c <= 0x7e) || c == 0x85 || (c >= 0xa0 && c <= 0xd7ff) || (c >= 0xe000 && c <= 0xfffd) || (c >= 0x10000 && c <= 0x10ffff)
}

// catCBOR renders the rows like Jackson's CBORGenerator (indefinite-length
// array and maps, definite-length text strings).
func catCBOR(headers []catDisplay, cells [][]*string) string {
	b := []byte{0x9f}
	text := func(s string) {
		n, chars := len(s), catLen(s)
		switch {
		case chars <= 255 && n <= 23:
			b = append(b, 0x60+byte(n))
		case chars <= 255 && n <= 0xff:
			b = append(b, 0x78, byte(n))
		case chars <= 255:
			b = append(b, 0x79, byte(n>>8), byte(n))
		default:
			b = append(b, 0x7a, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		}
		b = append(b, s...)
	}
	for _, row := range cells {
		b = append(b, 0xbf)
		for j, v := range row {
			text(headers[j].display)
			if v == nil {
				b = append(b, 0xf6)
			} else {
				text(*v)
			}
		}
		b = append(b, 0xff)
	}
	return string(append(b, 0xff))
}

// catSmile renders the rows like Jackson's SmileGenerator with OpenSearch's
// settings (shared names, raw binary, no shared string values).
func catSmile(headers []catDisplay, cells [][]*string) string {
	b := []byte{':', ')', '\n', 0x05, 0xf8}
	seen := map[string]int{}
	for _, row := range cells {
		b = append(b, 0xfa)
		for j, v := range row {
			name := headers[j].display
			switch ix, ok := seen[name]; {
			case name == "":
				b = append(b, 0x20)
			case ok && ix < 64:
				b = append(b, 0x40+byte(ix))
			case ok:
				b = append(b, 0x30+byte(ix>>8), byte(ix))
			default:
				ascii := len(name) == catLen(name)
				switch {
				case ascii && len(name) <= 64:
					b = append(b, 0x80+byte(len(name)-1))
					b = append(b, name...)
				case !ascii && len(name) <= 56:
					b = append(b, 0xc0+byte(len(name)-2))
					b = append(b, name...)
				default:
					b = append(b, 0x34)
					b = append(b, name...)
					b = append(b, 0xfc)
				}
				if len(seen) == 1024 {
					seen = map[string]int{}
				}
				seen[name] = len(seen)
			}
			switch {
			case v == nil:
				b = append(b, 0x21)
			case *v == "":
				b = append(b, 0x20)
			default:
				s := *v
				ascii := len(s) == catLen(s) && catLen(s) == len([]rune(s))
				switch {
				case ascii && len(s) <= 64:
					b = append(b, 0x3f+byte(len(s)))
					b = append(b, s...)
				case !ascii && catLen(s) <= 64 && len(s) <= 64:
					b = append(b, 0x7e+byte(len(s)))
					b = append(b, s...)
				case ascii:
					b = append(b, 0xe0)
					b = append(b, s...)
					b = append(b, 0xfc)
				default:
					b = append(b, 0xe4)
					b = append(b, s...)
					b = append(b, 0xfc)
				}
			}
		}
		b = append(b, 0xfb)
	}
	return string(append(b, 0xf9))
}

// catSplitComma is Strings.splitStringByCommaToArray (String.split drops
// trailing empty strings).
func catSplitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

var catIndicesColumns = catColumns(
	"health;alias:h;desc:current health status",
	"status;alias:s;desc:open/close status",
	"index;alias:i,idx;desc:index name",
	"uuid;alias:id,uuid;desc:index uuid",
	"pri;alias:p,shards.primary,shardsPrimary;text-align:right;desc:number of primary shards",
	"rep;alias:r,shards.replica,shardsReplica;text-align:right;desc:number of replica shards",
	"docs.count;alias:dc,docsCount;text-align:right;desc:available docs",
	"docs.deleted;alias:dd,docsDeleted;text-align:right;desc:deleted docs",
	"creation.date;alias:cd;default:false;desc:index creation date (millisecond value)",
	"creation.date.string;alias:cds;default:false;desc:index creation date (as string)",
	"store.size;alias:ss,storeSize;text-align:right;sibling:pri;desc:store size of primaries & replicas",
	"pri.store.size;text-align:right;desc:store size of primaries",
	"completion.size;alias:cs,completionSize;default:false;text-align:right;sibling:pri;desc:size of completion",
	"pri.completion.size;default:false;text-align:right;desc:size of completion",
	"fielddata.memory_size;alias:fm,fielddataMemory;default:false;text-align:right;sibling:pri;desc:used fielddata cache",
	"pri.fielddata.memory_size;default:false;text-align:right;desc:used fielddata cache",
	"fielddata.evictions;alias:fe,fielddataEvictions;default:false;text-align:right;sibling:pri;desc:fielddata evictions",
	"pri.fielddata.evictions;default:false;text-align:right;desc:fielddata evictions",
	"query_cache.memory_size;alias:qcm,queryCacheMemory;default:false;text-align:right;sibling:pri;desc:used query cache",
	"pri.query_cache.memory_size;default:false;text-align:right;desc:used query cache",
	"query_cache.evictions;alias:qce,queryCacheEvictions;default:false;text-align:right;sibling:pri;desc:query cache evictions",
	"pri.query_cache.evictions;default:false;text-align:right;desc:query cache evictions",
	"request_cache.memory_size;alias:rcm,requestCacheMemory;default:false;text-align:right;sibling:pri;desc:used request cache",
	"pri.request_cache.memory_size;default:false;text-align:right;desc:used request cache",
	"request_cache.evictions;alias:rce,requestCacheEvictions;default:false;text-align:right;sibling:pri;desc:request cache evictions",
	"pri.request_cache.evictions;default:false;text-align:right;desc:request cache evictions",
	"request_cache.hit_count;alias:rchc,requestCacheHitCount;default:false;text-align:right;sibling:pri;desc:request cache hit count",
	"pri.request_cache.hit_count;default:false;text-align:right;desc:request cache hit count",
	"request_cache.miss_count;alias:rcmc,requestCacheMissCount;default:false;text-align:right;sibling:pri;desc:request cache miss count",
	"pri.request_cache.miss_count;default:false;text-align:right;desc:request cache miss count",
	"flush.total;alias:ft,flushTotal;default:false;text-align:right;sibling:pri;desc:number of flushes",
	"pri.flush.total;default:false;text-align:right;desc:number of flushes",
	"flush.total_time;alias:ftt,flushTotalTime;default:false;text-align:right;sibling:pri;desc:time spent in flush",
	"pri.flush.total_time;default:false;text-align:right;desc:time spent in flush",
	"get.current;alias:gc,getCurrent;default:false;text-align:right;sibling:pri;desc:number of current get ops",
	"pri.get.current;default:false;text-align:right;desc:number of current get ops",
	"get.time;alias:gti,getTime;default:false;text-align:right;sibling:pri;desc:time spent in get",
	"pri.get.time;default:false;text-align:right;desc:time spent in get",
	"get.total;alias:gto,getTotal;default:false;text-align:right;sibling:pri;desc:number of get ops",
	"pri.get.total;default:false;text-align:right;desc:number of get ops",
	"get.exists_time;alias:geti,getExistsTime;default:false;text-align:right;sibling:pri;desc:time spent in successful gets",
	"pri.get.exists_time;default:false;text-align:right;desc:time spent in successful gets",
	"get.exists_total;alias:geto,getExistsTotal;default:false;text-align:right;sibling:pri;desc:number of successful gets",
	"pri.get.exists_total;default:false;text-align:right;desc:number of successful gets",
	"get.missing_time;alias:gmti,getMissingTime;default:false;text-align:right;sibling:pri;desc:time spent in failed gets",
	"pri.get.missing_time;default:false;text-align:right;desc:time spent in failed gets",
	"get.missing_total;alias:gmto,getMissingTotal;default:false;text-align:right;sibling:pri;desc:number of failed gets",
	"pri.get.missing_total;default:false;text-align:right;desc:number of failed gets",
	"indexing.delete_current;alias:idc,indexingDeleteCurrent;default:false;text-align:right;sibling:pri;desc:number of current deletions",
	"pri.indexing.delete_current;default:false;text-align:right;desc:number of current deletions",
	"indexing.delete_time;alias:idti,indexingDeleteTime;default:false;text-align:right;sibling:pri;desc:time spent in deletions",
	"pri.indexing.delete_time;default:false;text-align:right;desc:time spent in deletions",
	"indexing.delete_total;alias:idto,indexingDeleteTotal;default:false;text-align:right;sibling:pri;desc:number of delete ops",
	"pri.indexing.delete_total;default:false;text-align:right;desc:number of delete ops",
	"indexing.index_current;alias:iic,indexingIndexCurrent;default:false;text-align:right;sibling:pri;desc:number of current indexing ops",
	"pri.indexing.index_current;default:false;text-align:right;desc:number of current indexing ops",
	"indexing.index_time;alias:iiti,indexingIndexTime;default:false;text-align:right;sibling:pri;desc:time spent in indexing",
	"pri.indexing.index_time;default:false;text-align:right;desc:time spent in indexing",
	"indexing.index_total;alias:iito,indexingIndexTotal;default:false;text-align:right;sibling:pri;desc:number of indexing ops",
	"pri.indexing.index_total;default:false;text-align:right;desc:number of indexing ops",
	"indexing.index_failed;alias:iif,indexingIndexFailed;default:false;text-align:right;sibling:pri;desc:number of failed indexing ops",
	"pri.indexing.index_failed;default:false;text-align:right;desc:number of failed indexing ops",
	"merges.current;alias:mc,mergesCurrent;default:false;text-align:right;sibling:pri;desc:number of current merges",
	"pri.merges.current;default:false;text-align:right;desc:number of current merges",
	"merges.current_docs;alias:mcd,mergesCurrentDocs;default:false;text-align:right;sibling:pri;desc:number of current merging docs",
	"pri.merges.current_docs;default:false;text-align:right;desc:number of current merging docs",
	"merges.current_size;alias:mcs,mergesCurrentSize;default:false;text-align:right;sibling:pri;desc:size of current merges",
	"pri.merges.current_size;default:false;text-align:right;desc:size of current merges",
	"merges.total;alias:mt,mergesTotal;default:false;text-align:right;sibling:pri;desc:number of completed merge ops",
	"pri.merges.total;default:false;text-align:right;desc:number of completed merge ops",
	"merges.total_docs;alias:mtd,mergesTotalDocs;default:false;text-align:right;sibling:pri;desc:docs merged",
	"pri.merges.total_docs;default:false;text-align:right;desc:docs merged",
	"merges.total_size;alias:mts,mergesTotalSize;default:false;text-align:right;sibling:pri;desc:size merged",
	"pri.merges.total_size;default:false;text-align:right;desc:size merged",
	"merges.total_time;alias:mtt,mergesTotalTime;default:false;text-align:right;sibling:pri;desc:time spent in merges",
	"pri.merges.total_time;default:false;text-align:right;desc:time spent in merges",
	"merges.warmer.total_invocations;alias:mswti,mergedSegmentWarmerTotalInvocations;default:false;text-align:right;desc:total invocations of merged segment warmer",
	"pri.merges.warmer.total_invocations;default:false;text-align:right;desc:total invocations of merged segment warmer",
	"merges.warmer.total_time;alias:mswtt,mergedSegmentWarmerTotalTime;default:false;text-align:right;desc:total wallclock time spent in the warming operation",
	"pri.merges.warmer.total_time;default:false;text-align:right;desc:total wallclock time spent in the warming operation",
	"merges.warmer.ongoing_count;alias:mswoc,mergedSegmentWarmerOngoingCount;default:false;text-align:right;desc:point-in-time metric for number of in-progress warm operations",
	"pri.merges.warmer.ongoing_count;default:false;text-align:right;desc:point-in-time metric for number of in-progress warm operations",
	"merges.warmer.total_bytes_received;alias:mswtbr,mergedSegmentWarmerTotalBytesReceived;default:false;text-align:right;desc:total bytes received by a replica shard during the warm operation",
	"pri.merges.warmer.total_bytes_received;default:false;text-align:right;desc:total bytes received by a replica shard during the warm operation",
	"merges.warmer.total_bytes_sent;alias:mswtbs,mergedSegmentWarmerTotalBytesSent;default:false;text-align:right;desc:total bytes sent by a primary shard during the warm operation",
	"pri.merges.warmer.total_bytes_sent;default:false;text-align:right;desc:total bytes sent by a primary shard during the warm operation",
	"merges.warmer.total_receive_time;alias:mswtrt,mergedSegmentWarmerTotalReceiveTime;default:false;text-align:right;desc:total wallclock time spent receiving merged segments by a replica shard",
	"pri.merges.warmer.total_receive_time;default:false;text-align:right;desc:total wallclock time spent receiving merged segments by a replica shard",
	"merges.warmer.total_failure_count;alias:mswtfc,mergedSegmentWarmerTotalFailureCount;default:false;text-align:right;desc:total failures in merged segment warmer",
	"pri.merges.warmer.total_failure_count;default:false;text-align:right;desc:total failures in merged segment warmer",
	"merges.warmer.total_send_time;alias:mswtst,mergedSegmentWarmerTotalSendTime;default:false;text-align:right;desc:total wallclock time spent sending merged segments by a primary shard",
	"pri.merges.warmer.total_send_time;default:false;text-align:right;desc:total wallclock time spent sending merged segments by a primary shard",
	"refresh.total;alias:rto,refreshTotal;default:false;text-align:right;sibling:pri;desc:total refreshes",
	"pri.refresh.total;default:false;text-align:right;desc:total refreshes",
	"refresh.time;alias:rti,refreshTime;default:false;text-align:right;sibling:pri;desc:time spent in refreshes",
	"pri.refresh.time;default:false;text-align:right;desc:time spent in refreshes",
	"refresh.external_total;alias:rto,refreshTotal;default:false;text-align:right;sibling:pri;desc:total external refreshes",
	"pri.refresh.external_total;default:false;text-align:right;desc:total external refreshes",
	"refresh.external_time;alias:rti,refreshTime;default:false;text-align:right;sibling:pri;desc:time spent in external refreshes",
	"pri.refresh.external_time;default:false;text-align:right;desc:time spent in external refreshes",
	"refresh.listeners;alias:rli,refreshListeners;default:false;text-align:right;sibling:pri;desc:number of pending refresh listeners",
	"pri.refresh.listeners;default:false;text-align:right;desc:number of pending refresh listeners",
	"search.fetch_current;alias:sfc,searchFetchCurrent;default:false;text-align:right;sibling:pri;desc:current fetch phase ops",
	"pri.search.fetch_current;default:false;text-align:right;desc:current fetch phase ops",
	"search.fetch_time;alias:sfti,searchFetchTime;default:false;text-align:right;sibling:pri;desc:time spent in fetch phase",
	"pri.search.fetch_time;default:false;text-align:right;desc:time spent in fetch phase",
	"search.fetch_total;alias:sfto,searchFetchTotal;default:false;text-align:right;sibling:pri;desc:total fetch ops",
	"pri.search.fetch_total;default:false;text-align:right;desc:total fetch ops",
	"search.open_contexts;alias:so,searchOpenContexts;default:false;text-align:right;sibling:pri;desc:open search contexts",
	"pri.search.open_contexts;default:false;text-align:right;desc:open search contexts",
	"search.query_current;alias:sqc,searchQueryCurrent;default:false;text-align:right;sibling:pri;desc:current query phase ops",
	"pri.search.query_current;default:false;text-align:right;desc:current query phase ops",
	"search.query_time;alias:sqti,searchQueryTime;default:false;text-align:right;sibling:pri;desc:time spent in query phase",
	"pri.search.query_time;default:false;text-align:right;desc:time spent in query phase",
	"search.query_total;alias:sqto,searchQueryTotal;default:false;text-align:right;sibling:pri;desc:total query phase ops",
	"pri.search.query_total;default:false;text-align:right;desc:total query phase ops",
	"search.query_failed;alias:sqf,searchQueryFailed;default:false;text-align:right;sibling:pri;desc:failed query phase ops",
	"pri.search.query_failed;default:false;text-align:right;desc:failed query phase ops",
	"search.concurrent_query_current;alias:scqc,searchConcurrentQueryCurrent;default:false;text-align:right;sibling:pri;desc:current concurrent query phase ops",
	"pri.search.concurrent_query_current;default:false;text-align:right;desc:current concurrent query phase ops",
	"search.concurrent_query_time;alias:scqti,searchConcurrentQueryTime;default:false;text-align:right;sibling:pri;desc:time spent in concurrent query phase",
	"pri.search.concurrent_query_time;default:false;text-align:right;desc:time spent in concurrent query phase",
	"search.concurrent_query_total;alias:scqto,searchConcurrentQueryTotal;default:false;text-align:right;sibling:pri;desc:total query phase ops",
	"pri.search.concurrent_query_total;default:false;text-align:right;desc:total query phase ops",
	"search.concurrent_avg_slice_count;alias:casc,searchConcurrentAvgSliceCount;default:false;text-align:right;sibling:pri;desc:average query concurrency",
	"pri.search.concurrent_avg_slice_count;default:false;text-align:right;desc:average query concurrency",
	"search.startree_query_current;alias:stqc,startreeQueryCurrent;default:false;text-align:right;desc:current star tree query ops",
	"pri.search.startree.query_current;default:false;text-align:right;desc:current star tree query ops",
	"search.startree_query_time;alias:stqti,startreeQueryTime;default:false;text-align:right;desc:time spent in star tree queries",
	"pri.search.startree.query_time;default:false;text-align:right;desc:time spent in star tree queries",
	"search.startree_query_failed;alias:stqf,startreeQueryFailed;default:false;text-align:right;sibling:pri;desc:failed star tree query phase ops",
	"pri.search.startree_query_failed;default:false;text-align:right;desc:failed star tree query phase ops",
	"search.startree_query_total;alias:stqto,startreeQueryCurrent;default:false;text-align:right;desc:total star tree resolved queries",
	"pri.search.startree.query_total;default:false;text-align:right;desc:total star tree resolved queries",
	"search.scroll_current;alias:scc,searchScrollCurrent;default:false;text-align:right;sibling:pri;desc:open scroll contexts",
	"pri.search.scroll_current;default:false;text-align:right;desc:open scroll contexts",
	"search.scroll_time;alias:scti,searchScrollTime;default:false;text-align:right;sibling:pri;desc:time scroll contexts held open",
	"pri.search.scroll_time;default:false;text-align:right;desc:time scroll contexts held open",
	"search.scroll_total;alias:scto,searchScrollTotal;default:false;text-align:right;sibling:pri;desc:completed scroll contexts",
	"pri.search.scroll_total;default:false;text-align:right;desc:completed scroll contexts",
	"search.point_in_time_current;alias:scc,searchPointInTimeCurrent;default:false;text-align:right;sibling:pri;desc:open point in time contexts",
	"pri.search.point_in_time_current;default:false;text-align:right;desc:open point in time contexts",
	"search.point_in_time_time;alias:scti,searchPointInTimeTime;default:false;text-align:right;sibling:pri;desc:time point in time contexts held open",
	"pri.search.point_in_time_time;default:false;text-align:right;desc:time point in time contexts held open",
	"search.point_in_time_total;alias:scto,searchPointInTimeTotal;default:false;text-align:right;sibling:pri;desc:completed point in time contexts",
	"pri.search.point_in_time_total;default:false;text-align:right;desc:completed point in time contexts",
	"segments.count;alias:sc,segmentsCount;default:false;text-align:right;sibling:pri;desc:number of segments",
	"pri.segments.count;default:false;text-align:right;desc:number of segments",
	"segments.memory;alias:sm,segmentsMemory;default:false;text-align:right;sibling:pri;desc:memory used by segments",
	"pri.segments.memory;default:false;text-align:right;desc:memory used by segments",
	"segments.index_writer_memory;alias:siwm,segmentsIndexWriterMemory;default:false;text-align:right;sibling:pri;desc:memory used by index writer",
	"pri.segments.index_writer_memory;default:false;text-align:right;desc:memory used by index writer",
	"segments.version_map_memory;alias:svmm,segmentsVersionMapMemory;default:false;text-align:right;sibling:pri;desc:memory used by version map",
	"pri.segments.version_map_memory;default:false;text-align:right;desc:memory used by version map",
	"segments.fixed_bitset_memory;alias:sfbm,fixedBitsetMemory;default:false;text-align:right;sibling:pri;desc:memory used by fixed bit sets for nested object field types and type filters for types referred in _parent fields",
	"pri.segments.fixed_bitset_memory;default:false;text-align:right;desc:memory used by fixed bit sets for nested object field types and type filters for types referred in _parent fields",
	"warmer.current;alias:wc,warmerCurrent;default:false;text-align:right;sibling:pri;desc:current warmer ops",
	"pri.warmer.current;default:false;text-align:right;desc:current warmer ops",
	"warmer.total;alias:wto,warmerTotal;default:false;text-align:right;sibling:pri;desc:total warmer ops",
	"pri.warmer.total;default:false;text-align:right;desc:total warmer ops",
	"warmer.total_time;alias:wtt,warmerTotalTime;default:false;text-align:right;sibling:pri;desc:time spent in warmers",
	"pri.warmer.total_time;default:false;text-align:right;desc:time spent in warmers",
	"suggest.current;alias:suc,suggestCurrent;default:false;text-align:right;sibling:pri;desc:number of current suggest ops",
	"pri.suggest.current;default:false;text-align:right;desc:number of current suggest ops",
	"suggest.time;alias:suti,suggestTime;default:false;text-align:right;sibling:pri;desc:time spend in suggest",
	"pri.suggest.time;default:false;text-align:right;desc:time spend in suggest",
	"suggest.total;alias:suto,suggestTotal;default:false;text-align:right;sibling:pri;desc:number of suggest ops",
	"pri.suggest.total;default:false;text-align:right;desc:number of suggest ops",
	"memory.total;alias:tm,memoryTotal;default:false;text-align:right;sibling:pri;desc:total used memory",
	"pri.memory.total;default:false;text-align:right;desc:total user memory",
	"search.throttled;alias:sth;default:false;desc:indicates if the index is search throttled",
	"last_index_request_timestamp;alias:last_index_ts,lastIndexRequestTimestamp;default:false;text-align:right;desc:timestamp of the last processed index request (epoch millis)",
	"last_index_request_timestamp_string;alias:last_index_ts_string,lastIndexRequestTimestampString;default:false;text-align:right;desc:timestamp of the last processed index request (ISO8601 string)",
)

var catAliasesColumns = catColumns(
	"alias;alias:a;desc:alias name",
	"index;alias:i,idx;desc:index alias points to",
	"filter;alias:f,fi;desc:filter",
	"routing.index;alias:ri,routingIndex;desc:index routing",
	"routing.search;alias:rs,routingSearch;desc:search routing",
	"is_write_index;alias:w,isWriteIndex;desc:write index",
)

var catCountColumns = catColumns(
	"epoch;alias:t,time;desc:seconds since 1970-01-01 00:00:00",
	"timestamp;alias:ts,hms,hhmmss;desc:time in HH:MM:SS",
	"count;alias:dc,docs.count,docsCount;desc:the document count",
)

var catHealthColumns = catColumns(
	"epoch;alias:t,time;desc:seconds since 1970-01-01 00:00:00",
	"timestamp;alias:ts,hms,hhmmss;desc:time in HH:MM:SS",
	"cluster;alias:cl;desc:cluster name",
	"status;alias:st;desc:health status",
	"node.total;alias:nt,nodeTotal;text-align:right;desc:total number of nodes",
	"node.data;alias:nd,nodeData;text-align:right;desc:number of nodes that can store data",
	"discovered_cluster_manager;alias:dcm,dm,discovered_master;text-align:right;desc:cluster manager is discovered or not",
	"shards;alias:t,sh,shards.total,shardsTotal;text-align:right;desc:total number of shards",
	"pri;alias:p,shards.primary,shardsPrimary;text-align:right;desc:number of primary shards",
	"relo;alias:r,shards.relocating,shardsRelocating;text-align:right;desc:number of relocating nodes",
	"init;alias:i,shards.initializing,shardsInitializing;text-align:right;desc:number of initializing nodes",
	"unassign;alias:u,shards.unassigned,shardsUnassigned;text-align:right;desc:number of unassigned shards",
	"pending_tasks;alias:pt,pendingTasks;text-align:right;desc:number of pending tasks",
	"max_task_wait_time;alias:mtwt,maxTaskWaitTime;text-align:right;desc:wait time of longest task pending",
	"active_shards_percent;alias:asp,activeShardsPercent;text-align:right;desc:active number of shards in percent",
)

var catNodesColumns = catColumns(
	"id;alias:id,nodeId;default:false;desc:unique node id",
	"pid;alias:p;default:false;desc:process id",
	"ip;alias:i;desc:ip address",
	"port;alias:po;default:false;desc:bound transport port",
	"http_address;alias:http;default:false;desc:bound http address",
	"version;alias:v;default:false;desc:os version",
	"type;alias:t;default:false;desc:os distribution type",
	"build;alias:b;default:false;desc:os build hash",
	"jdk;alias:j;default:false;desc:jdk version",
	"disk.total;alias:dt,diskTotal;default:false;text-align:right;desc:total disk space",
	"disk.used;alias:du,diskUsed;default:false;text-align:right;desc:used disk space",
	"disk.avail;alias:d,da,disk,diskAvail;default:false;text-align:right;desc:available disk space",
	"disk.used_percent;alias:dup,diskUsedPercent;default:false;text-align:right;desc:used disk space percentage",
	"heap.current;alias:hc,heapCurrent;default:false;text-align:right;desc:used heap",
	"heap.percent;alias:hp,heapPercent;text-align:right;desc:used heap ratio",
	"heap.max;alias:hm,heapMax;default:false;text-align:right;desc:max configured heap",
	"ram.current;alias:rc,ramCurrent;default:false;text-align:right;desc:used machine memory",
	"ram.percent;alias:rp,ramPercent;text-align:right;desc:used machine memory ratio",
	"ram.max;alias:rm,ramMax;default:false;text-align:right;desc:total machine memory",
	"file_desc.current;alias:fdc,fileDescriptorCurrent;default:false;text-align:right;desc:used file descriptors",
	"file_desc.percent;alias:fdp,fileDescriptorPercent;default:false;text-align:right;desc:used file descriptor ratio",
	"file_desc.max;alias:fdm,fileDescriptorMax;default:false;text-align:right;desc:max file descriptors",
	"cpu;alias:cpu;text-align:right;desc:recent cpu usage",
	"load_1m;alias:l;text-align:right;desc:1m load avg",
	"load_5m;alias:l;text-align:right;desc:5m load avg",
	"load_15m;alias:l;text-align:right;desc:15m load avg",
	"uptime;alias:u;default:false;text-align:right;desc:node uptime",
	"node.role;alias:r,role,nodeRole;desc:m:master eligible node, d:data node, i:ingest node, -:coordinating node only",
	"node.roles;alias:rs,all roles;desc: -:coordinating node only",
	"cluster_manager;alias:cm,m,master;desc:*:current cluster manager",
	"name;alias:n;desc:node name",
	"completion.size;alias:cs,completionSize;default:false;text-align:right;desc:size of completion",
	"fielddata.memory_size;alias:fm,fielddataMemory;default:false;text-align:right;desc:used fielddata cache",
	"fielddata.evictions;alias:fe,fielddataEvictions;default:false;text-align:right;desc:fielddata evictions",
	"query_cache.memory_size;alias:qcm,queryCacheMemory;default:false;text-align:right;desc:used query cache",
	"query_cache.evictions;alias:qce,queryCacheEvictions;default:false;text-align:right;desc:query cache evictions",
	"query_cache.hit_count;alias:qchc,queryCacheHitCount;default:false;text-align:right;desc:query cache hit counts",
	"query_cache.miss_count;alias:qcmc,queryCacheMissCount;default:false;text-align:right;desc:query cache miss counts",
	"request_cache.memory_size;alias:rcm,requestCacheMemory;default:false;text-align:right;desc:used request cache",
	"request_cache.evictions;alias:rce,requestCacheEvictions;default:false;text-align:right;desc:request cache evictions",
	"request_cache.hit_count;alias:rchc,requestCacheHitCount;default:false;text-align:right;desc:request cache hit counts",
	"request_cache.miss_count;alias:rcmc,requestCacheMissCount;default:false;text-align:right;desc:request cache miss counts",
	"flush.total;alias:ft,flushTotal;default:false;text-align:right;desc:number of flushes",
	"flush.total_time;alias:ftt,flushTotalTime;default:false;text-align:right;desc:time spent in flush",
	"get.current;alias:gc,getCurrent;default:false;text-align:right;desc:number of current get ops",
	"get.time;alias:gti,getTime;default:false;text-align:right;desc:time spent in get",
	"get.total;alias:gto,getTotal;default:false;text-align:right;desc:number of get ops",
	"get.exists_time;alias:geti,getExistsTime;default:false;text-align:right;desc:time spent in successful gets",
	"get.exists_total;alias:geto,getExistsTotal;default:false;text-align:right;desc:number of successful gets",
	"get.missing_time;alias:gmti,getMissingTime;default:false;text-align:right;desc:time spent in failed gets",
	"get.missing_total;alias:gmto,getMissingTotal;default:false;text-align:right;desc:number of failed gets",
	"indexing.delete_current;alias:idc,indexingDeleteCurrent;default:false;text-align:right;desc:number of current deletions",
	"indexing.delete_time;alias:idti,indexingDeleteTime;default:false;text-align:right;desc:time spent in deletions",
	"indexing.delete_total;alias:idto,indexingDeleteTotal;default:false;text-align:right;desc:number of delete ops",
	"indexing.index_current;alias:iic,indexingIndexCurrent;default:false;text-align:right;desc:number of current indexing ops",
	"indexing.index_time;alias:iiti,indexingIndexTime;default:false;text-align:right;desc:time spent in indexing",
	"indexing.index_total;alias:iito,indexingIndexTotal;default:false;text-align:right;desc:number of indexing ops",
	"indexing.index_failed;alias:iif,indexingIndexFailed;default:false;text-align:right;desc:number of failed indexing ops",
	"merges.current;alias:mc,mergesCurrent;default:false;text-align:right;desc:number of current merges",
	"merges.current_docs;alias:mcd,mergesCurrentDocs;default:false;text-align:right;desc:number of current merging docs",
	"merges.current_size;alias:mcs,mergesCurrentSize;default:false;text-align:right;desc:size of current merges",
	"merges.total;alias:mt,mergesTotal;default:false;text-align:right;desc:number of completed merge ops",
	"merges.total_docs;alias:mtd,mergesTotalDocs;default:false;text-align:right;desc:docs merged",
	"merges.total_size;alias:mts,mergesTotalSize;default:false;text-align:right;desc:size merged",
	"merges.total_time;alias:mtt,mergesTotalTime;default:false;text-align:right;desc:time spent in merges",
	"merges.warmer.total_invocations;alias:mswti,mergedSegmentWarmerTotalInvocations;default:false;text-align:right;desc:total invocations of merged segment warmer",
	"merges.warmer.total_time;alias:mswtt,mergedSegmentWarmerTotalTime;default:false;text-align:right;desc:total wallclock time spent in the warming operation",
	"merges.warmer.ongoing_count;alias:mswoc,mergedSegmentWarmerOngoingCount;default:false;text-align:right;desc:point-in-time metric for number of in-progress warm operations",
	"merges.warmer.total_bytes_received;alias:mswtbr,mergedSegmentWarmerTotalBytesReceived;default:false;text-align:right;desc:total bytes received by a replica shard during the warm operation",
	"merges.warmer.total_bytes_sent;alias:mswtbs,mergedSegmentWarmerTotalBytesSent;default:false;text-align:right;desc:total bytes sent by a primary shard during the warm operation",
	"merges.warmer.total_receive_time;alias:mswtrt,mergedSegmentWarmerTotalReceiveTime;default:false;text-align:right;desc:total wallclock time spent receiving merged segments by a replica shard",
	"merges.warmer.total_failure_count;alias:mswtfc,mergedSegmentWarmerTotalFailureCount;default:false;text-align:right;desc:total failures in merged segment warmer",
	"merges.warmer.total_send_time;alias:mswtst,mergedSegmentWarmerTotalSendTime;default:false;text-align:right;desc:total wallclock time spent sending merged segments by a primary shard",
	"refresh.total;alias:rto,refreshTotal;default:false;text-align:right;desc:total refreshes",
	"refresh.time;alias:rti,refreshTime;default:false;text-align:right;desc:time spent in refreshes",
	"refresh.external_total;alias:rto,refreshTotal;default:false;text-align:right;desc:total external refreshes",
	"refresh.external_time;alias:rti,refreshTime;default:false;text-align:right;desc:time spent in external refreshes",
	"refresh.listeners;alias:rli,refreshListeners;default:false;text-align:right;desc:number of pending refresh listeners",
	"script.compilations;alias:scrcc,scriptCompilations;default:false;text-align:right;desc:script compilations",
	"script.cache_evictions;alias:scrce,scriptCacheEvictions;default:false;text-align:right;desc:script cache evictions",
	"script.compilation_limit_triggered;alias:scrclt,scriptCacheCompilationLimitTriggered;default:false;text-align:right;desc:script cache compilation limit triggered",
	"search.fetch_current;alias:sfc,searchFetchCurrent;default:false;text-align:right;desc:current fetch phase ops",
	"search.fetch_time;alias:sfti,searchFetchTime;default:false;text-align:right;desc:time spent in fetch phase",
	"search.fetch_total;alias:sfto,searchFetchTotal;default:false;text-align:right;desc:total fetch ops",
	"search.open_contexts;alias:so,searchOpenContexts;default:false;text-align:right;desc:open search contexts",
	"search.query_current;alias:sqc,searchQueryCurrent;default:false;text-align:right;desc:current query phase ops",
	"search.query_time;alias:sqti,searchQueryTime;default:false;text-align:right;desc:time spent in query phase",
	"search.query_total;alias:sqto,searchQueryTotal;default:false;text-align:right;desc:total query phase ops",
	"search.query_failed;alias:sqf,searchQueryFailed;default:false;text-align:right;desc:total failed query phase ops",
	"search.concurrent_query_current;alias:scqc,searchConcurrentQueryCurrent;default:false;text-align:right;desc:current concurrent query phase ops",
	"search.concurrent_query_time;alias:scqti,searchConcurrentQueryTime;default:false;text-align:right;desc:time spent in concurrent query phase",
	"search.concurrent_query_total;alias:scqto,searchConcurrentQueryTotal;default:false;text-align:right;desc:total concurrent query phase ops",
	"search.concurrent_avg_slice_count;alias:casc,searchConcurrentAvgSliceCount;default:false;text-align:right;desc:average query concurrency",
	"search.scroll_current;alias:scc,searchScrollCurrent;default:false;text-align:right;desc:open scroll contexts",
	"search.scroll_time;alias:scti,searchScrollTime;default:false;text-align:right;desc:time scroll contexts held open",
	"search.scroll_total;alias:scto,searchScrollTotal;default:false;text-align:right;desc:completed scroll contexts",
	"search.point_in_time_current;alias:scc,searchPointInTimeCurrent;default:false;text-align:right;desc:open point in time contexts",
	"search.point_in_time_time;alias:scti,searchPointInTimeTime;default:false;text-align:right;desc:time point in time contexts held open",
	"search.point_in_time_total;alias:scto,searchPointInTimeTotal;default:false;text-align:right;desc:completed point in time contexts",
	"search.startree_query_current;alias:stqc,startreeQueryCurrent;default:false;text-align:right;desc:current star tree query ops",
	"search.startree_query_time;alias:stqti,startreeQueryTime;default:false;text-align:right;desc:time spent in star tree queries",
	"search.startree_query_total;alias:stqto,startreeQueryTotal;default:false;text-align:right;desc:total star tree resolved queries",
	"search.startree_query_failed;alias:stqf,startreeQueryFailed;default:false;text-align:right;desc:star tree failed query ops",
	"segments.count;alias:sc,segmentsCount;default:false;text-align:right;desc:number of segments",
	"segments.memory;alias:sm,segmentsMemory;default:false;text-align:right;desc:memory used by segments",
	"segments.index_writer_memory;alias:siwm,segmentsIndexWriterMemory;default:false;text-align:right;desc:memory used by index writer",
	"segments.version_map_memory;alias:svmm,segmentsVersionMapMemory;default:false;text-align:right;desc:memory used by version map",
	"segments.fixed_bitset_memory;alias:sfbm,fixedBitsetMemory;default:false;text-align:right;desc:memory used by fixed bit sets for nested object field types and type filters for types referred in _parent fields",
	"suggest.current;alias:suc,suggestCurrent;default:false;text-align:right;desc:number of current suggest ops",
	"suggest.time;alias:suti,suggestTime;default:false;text-align:right;desc:time spend in suggest",
	"suggest.total;alias:suto,suggestTotal;default:false;text-align:right;desc:number of suggest ops",
)

var catMasterColumns = catColumns(
	"id;desc:node id",
	"host;alias:h;desc:host name",
	"ip;desc:ip address ",
	"node;alias:n;desc:node name",
)

var catPluginsColumns = catColumns(
	"id;default:false;desc:unique node id",
	"name;alias:n;desc:node name",
	"component;alias:c;desc:component",
	"version;alias:v;desc:component version",
	"description;alias:d;default:false;desc:plugin details",
)

var catTemplatesColumns = catColumns(
	"name;alias:n;desc:template name",
	"index_patterns;alias:t;desc:template index patterns",
	"order;alias:o,p;desc:template application order/priority number",
	"version;alias:v;desc:version",
	"composed_of;alias:c;desc:component templates comprising index template",
)

var catShardsColumns = catColumns(
	"index;alias:i,idx;desc:index name",
	"shard;alias:s,sh;desc:shard name",
	"prirep;alias:p,pr,primaryOrReplica;desc:primary or replica",
	"state;alias:st;desc:shard state",
	"docs;alias:d,dc;text-align:right;desc:number of docs in shard",
	"store;alias:sto;text-align:right;desc:store size of shard (how much disk it uses)",
	"ip;desc:ip of node where it lives",
	"id;default:false;desc:unique id of node where it lives",
	"node;alias:n;desc:name of node where it lives",
	"sync_id;alias:sync_id;default:false;desc:sync id",
	"unassigned.reason;alias:ur;default:false;desc:reason shard is unassigned",
	"unassigned.at;alias:ua;default:false;desc:time shard became unassigned (UTC)",
	"unassigned.for;alias:uf;default:false;text-align:right;desc:time has been unassigned",
	"unassigned.details;alias:ud;default:false;desc:additional details as to why the shard became unassigned",
	"recoverysource.type;alias:rs;default:false;desc:recovery source type",
	"completion.size;alias:cs,completionSize;default:false;text-align:right;desc:size of completion",
	"fielddata.memory_size;alias:fm,fielddataMemory;default:false;text-align:right;desc:used fielddata cache",
	"fielddata.evictions;alias:fe,fielddataEvictions;default:false;text-align:right;desc:fielddata evictions",
	"query_cache.memory_size;alias:qcm,queryCacheMemory;default:false;text-align:right;desc:used query cache",
	"query_cache.evictions;alias:qce,queryCacheEvictions;default:false;text-align:right;desc:query cache evictions",
	"flush.total;alias:ft,flushTotal;default:false;text-align:right;desc:number of flushes",
	"flush.total_time;alias:ftt,flushTotalTime;default:false;text-align:right;desc:time spent in flush",
	"get.current;alias:gc,getCurrent;default:false;text-align:right;desc:number of current get ops",
	"get.time;alias:gti,getTime;default:false;text-align:right;desc:time spent in get",
	"get.total;alias:gto,getTotal;default:false;text-align:right;desc:number of get ops",
	"get.exists_time;alias:geti,getExistsTime;default:false;text-align:right;desc:time spent in successful gets",
	"get.exists_total;alias:geto,getExistsTotal;default:false;text-align:right;desc:number of successful gets",
	"get.missing_time;alias:gmti,getMissingTime;default:false;text-align:right;desc:time spent in failed gets",
	"get.missing_total;alias:gmto,getMissingTotal;default:false;text-align:right;desc:number of failed gets",
	"indexing.delete_current;alias:idc,indexingDeleteCurrent;default:false;text-align:right;desc:number of current deletions",
	"indexing.delete_time;alias:idti,indexingDeleteTime;default:false;text-align:right;desc:time spent in deletions",
	"indexing.delete_total;alias:idto,indexingDeleteTotal;default:false;text-align:right;desc:number of delete ops",
	"indexing.index_current;alias:iic,indexingIndexCurrent;default:false;text-align:right;desc:number of current indexing ops",
	"indexing.index_time;alias:iiti,indexingIndexTime;default:false;text-align:right;desc:time spent in indexing",
	"indexing.index_total;alias:iito,indexingIndexTotal;default:false;text-align:right;desc:number of indexing ops",
	"indexing.index_failed;alias:iif,indexingIndexFailed;default:false;text-align:right;desc:number of failed indexing ops",
	"merges.current;alias:mc,mergesCurrent;default:false;text-align:right;desc:number of current merges",
	"merges.current_docs;alias:mcd,mergesCurrentDocs;default:false;text-align:right;desc:number of current merging docs",
	"merges.current_size;alias:mcs,mergesCurrentSize;default:false;text-align:right;desc:size of current merges",
	"merges.total;alias:mt,mergesTotal;default:false;text-align:right;desc:number of completed merge ops",
	"merges.total_docs;alias:mtd,mergesTotalDocs;default:false;text-align:right;desc:docs merged",
	"merges.total_size;alias:mts,mergesTotalSize;default:false;text-align:right;desc:size merged",
	"merges.total_time;alias:mtt,mergesTotalTime;default:false;text-align:right;desc:time spent in merges",
	"merges.warmer.total_invocations;alias:mswti,mergedSegmentWarmerTotalInvocations;default:false;text-align:right;desc:total invocations of merged segment warmer",
	"merges.warmer.total_time;alias:mswtt,mergedSegmentWarmerTotalTime;default:false;text-align:right;desc:total wallclock time spent in the warming operation",
	"merges.warmer.ongoing_count;alias:mswoc,mergedSegmentWarmerOngoingCount;default:false;text-align:right;desc:point-in-time metric for number of in-progress warm operations",
	"merges.warmer.total_bytes_received;alias:mswtbr,mergedSegmentWarmerTotalBytesReceived;default:false;text-align:right;desc:total bytes received by a replica shard during the warm operation",
	"merges.warmer.total_bytes_sent;alias:mswtbs,mergedSegmentWarmerTotalBytesSent;default:false;text-align:right;desc:total bytes sent by a primary shard during the warm operation",
	"merges.warmer.total_receive_time;alias:mswtrt,mergedSegmentWarmerTotalReceiveTime;default:false;text-align:right;desc:total wallclock time spent receiving merged segments by a replica shard",
	"merges.warmer.total_failure_count;alias:mswtfc,mergedSegmentWarmerTotalFailureCount;default:false;text-align:right;desc:total failures in merged segment warmer",
	"merges.warmer.total_send_time;alias:mswtst,mergedSegmentWarmerTotalSendTime;default:false;text-align:right;desc:total wallclock time spent sending merged segments by a primary shard",
	"refresh.total;alias:rto,refreshTotal;default:false;text-align:right;desc:total refreshes",
	"refresh.time;alias:rti,refreshTime;default:false;text-align:right;desc:time spent in refreshes",
	"refresh.external_total;alias:rto,refreshTotal;default:false;text-align:right;desc:total external refreshes",
	"refresh.external_time;alias:rti,refreshTime;default:false;text-align:right;desc:time spent in external refreshes",
	"refresh.listeners;alias:rli,refreshListeners;default:false;text-align:right;desc:number of pending refresh listeners",
	"search.fetch_current;alias:sfc,searchFetchCurrent;default:false;text-align:right;desc:current fetch phase ops",
	"search.fetch_time;alias:sfti,searchFetchTime;default:false;text-align:right;desc:time spent in fetch phase",
	"search.fetch_total;alias:sfto,searchFetchTotal;default:false;text-align:right;desc:total fetch ops",
	"search.open_contexts;alias:so,searchOpenContexts;default:false;text-align:right;desc:open search contexts",
	"search.query_current;alias:sqc,searchQueryCurrent;default:false;text-align:right;desc:current query phase ops",
	"search.query_time;alias:sqti,searchQueryTime;default:false;text-align:right;desc:time spent in query phase",
	"search.query_total;alias:sqto,searchQueryTotal;default:false;text-align:right;desc:total query phase ops",
	"search.query_failed;alias:sqf,searchQueryFailed;default:false;text-align:right;desc:failed query phase ops",
	"search.concurrent_query_current;alias:scqc,searchConcurrentQueryCurrent;default:false;text-align:right;desc:current concurrent query phase ops",
	"search.concurrent_query_time;alias:scqti,searchConcurrentQueryTime;default:false;text-align:right;desc:time spent in concurrent query phase",
	"search.concurrent_query_total;alias:scqto,searchConcurrentQueryTotal;default:false;text-align:right;desc:total concurrent query phase ops",
	"search.concurrent_avg_slice_count;alias:casc,searchConcurrentAvgSliceCount;default:false;text-align:right;desc:average query concurrency",
	"search.startree_query_current;alias:stqc,startreeQueryCurrent;default:false;text-align:right;desc:current star tree query ops",
	"search.startree_query_time;alias:stqti,startreeQueryTime;default:false;text-align:right;desc:time spent in star tree queries",
	"search.startree_query_total;alias:stqto,startreeQueryTotal;default:false;text-align:right;desc:total star tree resolved queries",
	"search.startree_query_failed;alias:stqf,startreeQueryFailed;default:false;text-align:right;desc:failed star tree query ops",
	"search.scroll_current;alias:scc,searchScrollCurrent;default:false;text-align:right;desc:open scroll contexts",
	"search.scroll_time;alias:scti,searchScrollTime;default:false;text-align:right;desc:time scroll contexts held open",
	"search.scroll_total;alias:scto,searchScrollTotal;default:false;text-align:right;desc:completed scroll contexts",
	"search.point_in_time_current;alias:spc,searchPointInTimeCurrent;default:false;text-align:right;desc:open point in time contexts",
	"search.point_in_time_time;alias:spti,searchPointInTimeTime;default:false;text-align:right;desc:time point in time contexts held open",
	"search.point_in_time_total;alias:spto,searchPointInTimeTotal;default:false;text-align:right;desc:completed point in time contexts",
	"search.search_idle_reactivate_count_total;alias:ssirct,searchSearchIdleReactivateCountTotal;default:false;text-align:right;desc:number of times a shard reactivated",
	"segments.count;alias:sc,segmentsCount;default:false;text-align:right;desc:number of segments",
	"segments.memory;alias:sm,segmentsMemory;default:false;text-align:right;desc:memory used by segments",
	"segments.index_writer_memory;alias:siwm,segmentsIndexWriterMemory;default:false;text-align:right;desc:memory used by index writer",
	"segments.version_map_memory;alias:svmm,segmentsVersionMapMemory;default:false;text-align:right;desc:memory used by version map",
	"segments.fixed_bitset_memory;alias:sfbm,fixedBitsetMemory;default:false;text-align:right;desc:memory used by fixed bit sets for nested object field types and type filters for types referred in _parent fields",
	"seq_no.max;alias:sqm,maxSeqNo;default:false;text-align:right;desc:max sequence number",
	"seq_no.local_checkpoint;alias:sql,localCheckpoint;default:false;text-align:right;desc:local checkpoint",
	"seq_no.global_checkpoint;alias:sqg,globalCheckpoint;default:false;text-align:right;desc:global checkpoint",
	"warmer.current;alias:wc,warmerCurrent;default:false;text-align:right;desc:current warmer ops",
	"warmer.total;alias:wto,warmerTotal;default:false;text-align:right;desc:total warmer ops",
	"warmer.total_time;alias:wtt,warmerTotalTime;default:false;text-align:right;desc:time spent in warmers",
	"path.data;alias:pd,dataPath;default:false;text-align:right;desc:shard data path",
	"path.state;alias:ps,statsPath;default:false;text-align:right;desc:shard state path",
	"docs.deleted;alias:dd,docsDeleted;default:false;text-align:right;desc:number of deleted docs in shard",
)

var catSegmentsColumns = catColumns(
	"index;alias:i,idx;desc:index name",
	"shard;alias:s,sh;desc:shard name",
	"prirep;alias:p,pr,primaryOrReplica;desc:primary or replica",
	"ip;desc:ip of node where it lives",
	"id;default:false;desc:unique id of node where it lives",
	"segment;alias:seg;desc:segment name",
	"generation;alias:g,gen;text-align:right;desc:segment generation",
	"docs.count;alias:dc,docsCount;text-align:right;desc:number of docs in segment",
	"docs.deleted;alias:dd,docsDeleted;text-align:right;desc:number of deleted docs in segment",
	"size;alias:si;text-align:right;desc:segment size in bytes",
	"size.memory;alias:sm,sizeMemory;text-align:right;desc:segment memory in bytes",
	"committed;alias:ic,isCommitted;desc:is segment committed",
	"searchable;alias:is,isSearchable;desc:is segment searched",
	"version;alias:v,ver;desc:version",
	"compound;alias:ico,isCompound;desc:is segment compound",
)

var catAllocationColumns = catColumns(
	"shards;alias:s;text-align:right;desc:number of shards on node",
	"disk.indices;alias:di,diskIndices;text-align:right;desc:disk used by OpenSearch indices",
	"disk.used;alias:du,diskUsed;text-align:right;desc:disk used (total, not just OpenSearch)",
	"disk.avail;alias:da,diskAvail;text-align:right;desc:disk available",
	"disk.total;alias:dt,diskTotal;text-align:right;desc:total capacity of all volumes",
	"disk.percent;alias:dp,diskPercent;text-align:right;desc:percent disk used",
	"host;alias:h;desc:host of node",
	"ip;desc:ip of node",
	"node;alias:n;desc:name of node",
)

var catThreadPoolColumns = catColumns(
	"node_name;alias:nn;desc:node name",
	"node_id;alias:id;default:false;desc:persistent node id",
	"ephemeral_node_id;alias:eid;default:false;desc:ephemeral node id",
	"pid;alias:p;default:false;desc:process id",
	"host;alias:h;default:false;desc:host name",
	"ip;alias:i;default:false;desc:ip address",
	"port;alias:po;default:false;desc:bound transport port",
	"name;alias:n;desc:thread pool name",
	"type;alias:t;default:false;desc:thread pool type",
	"active;alias:a;text-align:right;desc:number of active threads",
	"pool_size;alias:psz;default:false;text-align:right;desc:number of threads",
	"queue;alias:q;text-align:right;desc:number of tasks currently in queue",
	"queue_size;alias:qs;default:false;text-align:right;desc:maximum number of tasks permitted in queue",
	"rejected;alias:r;text-align:right;desc:number of rejected tasks",
	"largest;alias:l;default:false;text-align:right;desc:highest number of seen active threads",
	"completed;alias:c;default:false;text-align:right;desc:number of completed tasks",
	"total_wait_time;alias:twt;default:false;text-align:right;desc:total time tasks spent waiting in thread_pool queue",
	"core;alias:cr;default:false;text-align:right;desc:core number of threads in a scaling thread pool",
	"max;alias:mx;default:false;text-align:right;desc:maximum number of threads in a scaling thread pool",
	"size;alias:sz;default:false;text-align:right;desc:number of threads in a fixed thread pool",
	"keep_alive;alias:ka;default:false;text-align:right;desc:thread keep alive time",
	"parallelism;alias:pl;default:false;text-align:right;desc:number of worker threads in a fork_join thread pool",
)

var catRecoveryColumns = catColumns(
	"index;alias:i,idx;desc:index name",
	"shard;alias:s,sh;desc:shard name",
	"start_time;alias:start;default:false;desc:recovery start time",
	"start_time_millis;alias:start_millis;default:false;desc:recovery start time in epoch milliseconds",
	"stop_time;alias:stop;default:false;desc:recovery stop time",
	"stop_time_millis;alias:stop_millis;default:false;desc:recovery stop time in epoch milliseconds",
	"time;alias:t,ti;desc:recovery time",
	"type;alias:ty;desc:recovery type",
	"stage;alias:st;desc:recovery stage",
	"source_host;alias:shost;desc:source host",
	"source_node;alias:snode;desc:source node name",
	"target_host;alias:thost;desc:target host",
	"target_node;alias:tnode;desc:target node name",
	"repository;alias:rep;desc:repository",
	"snapshot;alias:snap;desc:snapshot",
	"files;alias:f;desc:number of files to recover",
	"files_recovered;alias:fr;desc:files recovered",
	"files_percent;alias:fp;desc:percent of files recovered",
	"files_total;alias:tf;desc:total number of files",
	"bytes;alias:b;desc:number of bytes to recover",
	"bytes_recovered;alias:br;desc:bytes recovered",
	"bytes_percent;alias:bp;desc:percent of bytes recovered",
	"bytes_total;alias:tb;desc:total number of bytes",
	"translog_ops;alias:to;desc:number of translog ops to recover",
	"translog_ops_recovered;alias:tor;desc:translog ops recovered",
	"translog_ops_percent;alias:top;desc:percent of translog ops recovered",
)

var catPendingTasksColumns = catColumns(
	"insertOrder;alias:o;text-align:right;desc:task insertion order",
	"timeInQueue;alias:t;text-align:right;desc:how long task has been in queue",
	"priority;alias:p;desc:task priority",
	"source;alias:s;desc:task source",
)

var catFielddataColumns = catColumns(
	"id;desc:node id",
	"host;alias:h;desc:host name",
	"ip;desc:ip address",
	"node;alias:n;desc:node name",
	"field;alias:f;desc:field name",
	"size;alias:s;text-align:right;desc:field data usage",
)

var catNodeattrsColumns = catColumns(
	"node;alias:name;desc:node name",
	"id;alias:id,nodeId;default:false;desc:unique node id",
	"pid;alias:p;default:false;desc:process id",
	"host;alias:h;desc:host name",
	"ip;alias:i;desc:ip address",
	"port;alias:po;default:false;desc:bound transport port",
	"attr;alias:attr.name;desc:attribute description",
	"value;alias:attr.value;desc:attribute value",
)

var catTasksColumns = catColumns(
	"id;default:false;desc:id of the task with the node",
	"action;alias:ac;desc:task action",
	"task_id;alias:ti;desc:unique task id",
	"parent_task_id;alias:pti;desc:parent task id",
	"type;alias:ty;desc:task type",
	"start_time;alias:start;desc:start time in ms",
	"timestamp;alias:ts,hms,hhmmss;desc:start time in HH:MM:SS",
	"running_time_ns;alias:time;default:false;desc:running time ns",
	"running_time;alias:time;desc:running time",
	"node_id;alias:ni;default:false;desc:unique node id",
	"ip;alias:i;desc:ip address",
	"port;alias:po;default:false;desc:bound transport port",
	"node;alias:n;desc:node name",
	"version;alias:v;default:false;desc:es version",
	"x_opaque_id;alias:x;default:false;desc:X-Opaque-ID header",
)

var catRepositoriesColumns = catColumns(
	"id;alias:id,repoId;desc:unique repository id",
	"type;alias:t,type;desc:repository type",
)

var catSegmentReplicationColumns = catColumns(
	"shardId;alias:s;desc: shard Id",
	"target_node;alias:tnode;desc:target node name",
	"target_host;alias:thost;desc:target host",
	"checkpoints_behind;alias:cpb;desc:checkpoints behind primary",
	"bytes_behind;alias:bb;desc:bytes behind primary",
	"current_lag;alias:clag;desc:ongoing time elapsed waiting for replica to catch up to primary",
	"last_completed_lag;alias:lcl;desc:time taken for replica to catch up to latest primary refresh",
	"rejected_requests;alias:rr;desc:count of rejected requests for the replication group",
)

var catSnapshotsColumns = catColumns(
	"id;alias:id,snapshot;desc:unique snapshot",
	"status;alias:s,status;text-align:right;desc:snapshot name",
	"start_epoch;alias:ste,startEpoch;desc:start time in seconds since 1970-01-01 00:00:00",
	"start_time;alias:sti,startTime;desc:start time in HH:MM:SS",
	"end_epoch;alias:ete,endEpoch;desc:end time in seconds since 1970-01-01 00:00:00",
	"end_time;alias:eti,endTime;desc:end time in HH:MM:SS",
	"duration;alias:dur,duration;text-align:right;desc:duration",
	"indices;alias:i,indices;text-align:right;desc:number of indices",
	"successful_shards;alias:ss,successful_shards;text-align:right;desc:number of successful shards",
	"failed_shards;alias:fs,failed_shards;text-align:right;desc:number of failed shards",
	"total_shards;alias:ts,total_shards;text-align:right;desc:number of total shards",
	"reason;alias:r,reason;default:false;desc:reason for failures",
)

// address is the HTTP address reported to clients that sniff nodes.
func (h *httpHandler) address() string {
	if h.c.HTTPAddress != "" {
		return h.c.HTTPAddress
	}
	return "127.0.0.1:9200"
}

// catNodeInfo identifies the single osmem node in node level tables.
type catNodeInfo struct {
	id, name, host, ip, httpAddress string
	port                            int
}

func (h *httpHandler) catNode() catNodeInfo {
	addr := h.address()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		host, portText = addr, "9200"
	}
	port, _ := strconv.Atoi(portText)
	return catNodeInfo{id: "osmem-node", name: "osmem-node", host: host, ip: host, httpAddress: addr, port: port}
}

// shortID is the node id as tables without full_id print it.
func (n catNodeInfo) shortID() string {
	if len(n.id) > 4 {
		return n.id[:4]
	}
	return n.id
}

// matches implements the node filters of /_cat/allocation/{nodes}.
func (n catNodeInfo) matches(filter string) bool {
	if filter == "" {
		return true
	}
	for _, f := range catSplitComma(filter) {
		switch f {
		case "_all", "*", "_local", "_cluster_manager", "_master":
			return true
		}
		for _, v := range []string{n.id, n.name, n.host, n.ip} {
			if globMatch(f, v) {
				return true
			}
		}
	}
	return false
}

var catStarted = time.Now()

func catISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// catAddTimestamp fills the epoch and timestamp columns
// (Table.startHeadersWithTimestamp).
func catAddTimestamp(row map[string]any, now time.Time) {
	row["epoch"] = now.Unix()
	row["timestamp"] = now.UTC().Format("15:04:05")
}

// catFillZeros sets every column that is not in the row yet (and not listed
// as unknown) to the zero value of its statistic.
func catFillZeros(row map[string]any, cols []catColumn, unknown ...string) {
	skip := map[string]bool{}
	for _, u := range unknown {
		skip[u] = true
	}
	for _, c := range cols {
		if _, ok := row[c.name]; ok || skip[c.name] {
			continue
		}
		switch n := c.name; {
		case strings.HasSuffix(n, "avg_slice_count"):
			row[n] = catDouble(0)
		case strings.HasSuffix(n, "time"):
			row[n] = catTime(0)
		case strings.HasSuffix(n, "size"), strings.HasSuffix(n, "memory"), strings.HasSuffix(n, "memory.total"), strings.HasSuffix(n, "bytes_received"), strings.HasSuffix(n, "bytes_sent"):
			row[n] = catBytes(0)
		default:
			row[n] = int64(0)
		}
	}
}

// catNumber reads a JSON number of an engine response.
func catNumber(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n, true
		}
	case nil:
		return 0, false
	}
	if n, err := strconv.ParseInt(fmt.Sprint(v), 10, 64); err == nil {
		return n, true
	}
	return 0, false
}

func catStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

func catIndexHealth(h *httpHandler, ix engine.CatIndex) string {
	if ix.Closed {
		return "red"
	}
	if res, err := h.c.Health(ix.Name, engine.Params{}); err == nil {
		if m, ok := res.Body.(M); ok {
			if s, ok := m["status"].(string); ok {
				return s
			}
		}
	}
	if ix.Replicas > 0 {
		return "yellow"
	}
	return "green"
}

var catIndicesUnknown = []string{"last_index_request_timestamp_string"}

func catIndices(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catIndicesColumns, "local", "health"); done {
		return res, err
	}
	for _, flag := range []string{"local", "include_unloaded_segments"} {
		if _, err := cr.boolean(flag, false); err != nil {
			return engine.Response{}, err
		}
	}
	infos, err := h.c.CatIndices(cr.get("index"), cr.engineParams())
	if err != nil {
		return engine.Response{}, err
	}
	healthFilter, filterHealth := cr.param("health")
	if filterHealth {
		switch healthFilter = strings.ToLower(healthFilter); healthFilter {
		case "green", "yellow", "red":
		default:
			return engine.Response{}, &engine.Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "unknown cluster health status [" + cr.get("health") + "]"}
		}
	}
	t := &catTable{cols: catIndicesColumns}
	for _, ix := range infos {
		health := catIndexHealth(h, ix)
		if filterHealth && health != healthFilter {
			continue
		}
		row := map[string]any{
			"health": health, "status": "open", "index": ix.Name, "uuid": ix.UUID, "pri": len(ix.Shards), "rep": ix.Replicas,
			"creation.date": ix.Created.UnixMilli(), "creation.date.string": catISO(ix.Created),
			"memory.total": catBytes(0), "pri.memory.total": catBytes(0), "search.throttled": false,
		}
		if ix.Closed {
			row["status"] = "close"
			t.rows = append(t.rows, row)
			continue
		}
		docs, size, segments := int64(0), catBytes(0), int64(0)
		for _, s := range ix.Shards {
			docs += int64(s.Docs)
			size += catBytes(s.Bytes)
			if s.Docs > 0 {
				segments++
			}
		}
		row["docs.count"], row["docs.deleted"] = docs, int64(0)
		// replicas are never assigned on the single node, so the store of
		// primaries is the whole store
		row["store.size"], row["pri.store.size"] = size, size
		row["segments.count"], row["pri.segments.count"] = segments, segments
		catFillZeros(row, catIndicesColumns, catIndicesUnknown...)
		t.rows = append(t.rows, row)
	}
	return catRender(cr, t)
}

var catShardsUnknown = []string{"sync_id", "unassigned.reason", "unassigned.at", "unassigned.for", "unassigned.details", "recoverysource.type", "path.data", "path.state"}

func catShards(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catShardsColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	infos, err := h.c.CatIndices(cr.get("index"), cr.engineParams())
	if err != nil {
		return engine.Response{}, err
	}
	node, now := h.catNode(), h.c.Now()
	t := &catTable{cols: catShardsColumns}
	for _, ix := range infos {
		if ix.Closed {
			return engine.Response{}, &engine.Error{Status: http.StatusBadRequest, Type: "index_closed_exception", Reason: "closed", Index: ix.Name}
		}
		for i, s := range ix.Shards {
			segments := int64(0)
			if s.Docs > 0 {
				segments = 1
			}
			row := map[string]any{
				"index": ix.Name, "shard": i, "prirep": "p", "state": "STARTED", "docs": int64(s.Docs), "store": catBytes(s.Bytes),
				"ip": node.ip, "id": node.id, "node": node.name, "segments.count": segments, "docs.deleted": int64(0),
				"seq_no.max": s.MaxSeqNo, "seq_no.local_checkpoint": s.MaxSeqNo, "seq_no.global_checkpoint": s.MaxSeqNo,
			}
			catFillZeros(row, catShardsColumns, catShardsUnknown...)
			t.rows = append(t.rows, row)
			for range ix.Replicas {
				t.rows = append(t.rows, map[string]any{
					"index": ix.Name, "shard": i, "prirep": "r", "state": "UNASSIGNED",
					"unassigned.reason": "INDEX_CREATED", "unassigned.at": catISO(ix.Created),
					"unassigned.for": catTime(now.Sub(ix.Created).Milliseconds() * int64(time.Millisecond)), "recoverysource.type": "peer",
				})
			}
		}
	}
	return catRender(cr, t)
}

// catLuceneVersion is the Lucene version OpenSearch 3.8 writes segments with.
const catLuceneVersion = "10.5.0"

func catSegments(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catSegmentsColumns); done {
		return res, err
	}
	infos, err := h.c.CatIndices(cr.get("index"), cr.engineParams())
	if err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	t := &catTable{cols: catSegmentsColumns}
	for _, ix := range infos {
		if ix.Closed {
			return engine.Response{}, &engine.Error{Status: http.StatusBadRequest, Type: "index_closed_exception", Reason: "closed", Index: ix.Name}
		}
		for i, s := range ix.Shards {
			if s.Docs == 0 {
				continue
			}
			// one committed-to-memory segment per non-empty primary
			t.rows = append(t.rows, map[string]any{
				"index": ix.Name, "shard": i, "prirep": "p", "ip": node.ip, "id": node.id, "segment": "_0", "generation": int64(0),
				"docs.count": s.Docs, "docs.deleted": 0, "size": catBytes(s.Bytes - 208), "size.memory": int64(0),
				"committed": false, "searchable": true, "version": catLuceneVersion, "compound": true,
			})
		}
	}
	return catRender(cr, t)
}

func catRecovery(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catRecoveryColumns); done {
		return res, err
	}
	activeOnly, err := cr.boolean("active_only", false)
	if err != nil {
		return engine.Response{}, err
	}
	if _, err := cr.boolean("detailed", false); err != nil {
		return engine.Response{}, err
	}
	infos, err := h.c.CatIndices(cr.get("index"), cr.engineParams())
	if err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	t := &catTable{cols: catRecoveryColumns}
	for _, ix := range infos {
		if ix.Closed || activeOnly {
			continue
		}
		for i := range ix.Shards {
			// every primary was recovered from an empty store at creation
			t.rows = append(t.rows, map[string]any{
				"index": ix.Name, "shard": i, "start_time": catISO(ix.Created), "start_time_millis": ix.Created.UnixMilli(),
				"stop_time": catISO(ix.Created), "stop_time_millis": ix.Created.UnixMilli(), "time": catTime(0),
				"type": "empty_store", "stage": "done", "source_host": "n/a", "source_node": "n/a",
				"target_host": node.host, "target_node": node.name, "repository": "n/a", "snapshot": "n/a",
				"files": 0, "files_recovered": 0, "files_percent": "0.0%", "files_total": 0,
				"bytes": catBytes(0), "bytes_recovered": catBytes(0), "bytes_percent": "0.0%", "bytes_total": catBytes(0),
				"translog_ops": 0, "translog_ops_recovered": 0, "translog_ops_percent": "100.0%",
			})
		}
	}
	return catRender(cr, t)
}

func catAllocation(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catAllocationColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	shards, unassigned, disk := 0, 0, catBytes(0)
	for _, ix := range h.c.CatAllIndices() {
		shards += len(ix.Shards)
		unassigned += len(ix.Shards) * ix.Replicas
		for _, s := range ix.Shards {
			disk += catBytes(s.Bytes)
		}
	}
	t := &catTable{cols: catAllocationColumns}
	if node.matches(cr.get("nodes")) {
		// disk usage of the host is not collected (null like a node without fs stats)
		t.rows = append(t.rows, map[string]any{"shards": shards, "disk.indices": disk, "host": node.host, "ip": node.ip, "node": node.name})
	}
	if unassigned > 0 {
		t.rows = append(t.rows, map[string]any{"shards": unassigned, "node": "UNASSIGNED"})
	}
	return catRender(cr, t)
}

// catThreadPool describes one of OpenSearch's built-in thread pools; sizes
// follow ThreadPool's defaults for the allocated processors.
type catThreadPoolInfo struct {
	name, kind      string
	size, core, max int
	queue           int
	keepAlive       time.Duration
}

var catThreadPools = sync.OnceValue(func() []catThreadPoolInfo {
	procs := runtime.NumCPU()
	bounded := func(v, lo, hi int) int { return min(max(v, lo), hi) }
	halfMaxFive := bounded((procs+1)/2, 1, 5)
	halfMaxTen := bounded((procs+1)/2, 1, 10)
	twice := 2 * procs
	search := procs*3/2 + 1
	fixed := func(name string, size, queue int) catThreadPoolInfo {
		return catThreadPoolInfo{name: name, kind: "fixed", size: size, queue: queue}
	}
	resizable := func(name string, size, queue int) catThreadPoolInfo {
		return catThreadPoolInfo{name: name, kind: "resizable", size: size, queue: queue}
	}
	scaling := func(name string, core, maxSize int, keepAlive time.Duration) catThreadPoolInfo {
		return catThreadPoolInfo{name: name, kind: "scaling", core: core, max: maxSize, queue: -1, keepAlive: keepAlive}
	}
	return []catThreadPoolInfo{
		fixed("analyze", 1, 16),
		scaling("fetch_shard_started", 1, twice, 5*time.Minute),
		scaling("fetch_shard_store", 1, twice, 5*time.Minute),
		scaling("flush", 1, halfMaxTen, 5*time.Minute),
		fixed("force_merge", max(1, procs/8), -1),
		scaling("generic", 4, bounded(4*procs, 128, 512), 30*time.Second),
		fixed("get", procs, 1000),
		resizable("index_searcher", twice, 1000),
		fixed("listener", halfMaxTen, -1),
		scaling("management", 1, bounded(procs, 1, 5), 5*time.Minute),
		scaling("merge", 1, procs, 5*time.Minute),
		scaling("refresh", 1, halfMaxTen, 5*time.Minute),
		scaling("remote_download", 1, twice, 5*time.Minute),
		scaling("remote_purge", 1, halfMaxFive, 5*time.Minute),
		scaling("remote_recovery", 1, twice, 5*time.Minute),
		scaling("remote_refresh_retry", 1, halfMaxTen, 5*time.Minute),
		fixed("remote_state_read", bounded(4*procs, 4, 32), 120000),
		resizable("search", search, 1000),
		resizable("search_throttled", 1, 100),
		scaling("snapshot", 1, halfMaxFive, 5*time.Minute),
		scaling("snapshot_deletion", 1, 64, 5*time.Minute),
		resizable("stream_search", search, 1000),
		fixed("system_read", halfMaxFive, 2000),
		fixed("system_write", halfMaxFive, 1000),
		fixed("translog_sync", 4*procs, 10000),
		scaling("translog_transfer", 1, halfMaxTen, 5*time.Minute),
		scaling("warmer", 1, halfMaxFive, 5*time.Minute),
		fixed("write", procs, 10000),
	}
})

func catThreadPool(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catThreadPoolColumns, "thread_pool_patterns"); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	patterns := catSplitComma(cr.get("thread_pool_patterns"))
	node := h.catNode()
	t := &catTable{cols: catThreadPoolColumns}
	for _, pool := range catThreadPools() {
		matched := len(patterns) == 0
		for _, p := range patterns {
			matched = matched || globMatch(p, pool.name)
		}
		if !matched {
			continue
		}
		row := map[string]any{
			"node_name": node.name, "node_id": node.id, "ephemeral_node_id": node.id, "pid": int64(os.Getpid()), "host": node.host, "ip": node.ip, "port": node.port,
			"name": pool.name, "type": pool.kind, "active": 0, "pool_size": 0, "queue": 0, "queue_size": pool.queue, "rejected": int64(0),
			"largest": 0, "completed": int64(0), "total_wait_time": catTime(-1),
		}
		if pool.kind == "scaling" {
			row["core"], row["max"], row["keep_alive"] = pool.core, pool.max, catTime(pool.keepAlive)
		} else {
			row["size"] = pool.size
		}
		t.rows = append(t.rows, row)
	}
	return catRender(cr, t)
}

func catAliases(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catAliasesColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	// Metadata.findAliases: patterns apply in order, "-pattern" removes
	// aliases matched so far
	var patterns []string
	for _, p := range strings.Split(cr.get("alias"), ",") {
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	matches := func(alias string) bool {
		matched := len(patterns) == 0
		for _, p := range patterns {
			if strings.HasPrefix(p, "-") {
				if matched {
					matched = !globMatch(p[1:], alias)
				}
			} else if !matched {
				matched = p == "_all" || globMatch(p, alias)
			}
		}
		return matched
	}
	t := &catTable{cols: catAliasesColumns}
	for _, row := range h.c.CatAliases("") {
		alias, _ := row["alias"].(string)
		if !matches(alias) {
			continue
		}
		out := map[string]any{}
		for _, c := range catAliasesColumns {
			if s, ok := row[c.name].(string); ok {
				out[c.name] = s
			} else if row[c.name] != nil {
				out[c.name] = fmt.Sprint(row[c.name])
			}
		}
		t.rows = append(t.rows, out)
	}
	// indices in name order, aliases sorted within an index
	sort.SliceStable(t.rows, func(i, j int) bool {
		if a, b := t.rows[i]["index"].(string), t.rows[j]["index"].(string); a != b {
			return a < b
		}
		return t.rows[i]["alias"].(string) < t.rows[j]["alias"].(string)
	})
	return catRender(cr, t)
}

func catHealth(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catHealthColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	res, err := h.c.Health("", engine.Params{})
	if err != nil {
		return engine.Response{}, err
	}
	hm, _ := res.Body.(M)
	num := func(key string) int {
		n, _ := catNumber(hm[key])
		return int(n)
	}
	percent, _ := hm["active_shards_percent_as_number"].(float64)
	if d, isDouble := hm["active_shards_percent_as_number"].(engine.Double); isDouble {
		percent = float64(d)
	}
	discovered, ok := hm["discovered_cluster_manager"].(bool)
	if !ok {
		discovered = true
	}
	row := map[string]any{
		"cluster": fmt.Sprint(hm["cluster_name"]), "status": fmt.Sprint(hm["status"]), "node.total": num("number_of_nodes"), "node.data": num("number_of_data_nodes"),
		"discovered_cluster_manager": discovered, "shards": num("active_shards"), "pri": num("active_primary_shards"), "relo": num("relocating_shards"),
		"init": num("initializing_shards"), "unassign": num("unassigned_shards"), "pending_tasks": num("number_of_pending_tasks"),
		"max_task_wait_time": "-", "active_shards_percent": catJavaPercent(percent),
	}
	if wait := num("task_max_waiting_in_queue_millis"); wait > 0 {
		row["max_task_wait_time"] = catTime(int64(wait) * int64(time.Millisecond))
	}
	catAddTimestamp(row, h.c.Now())
	return catRender(cr, &catTable{cols: catHealthColumns, rows: []map[string]any{row}})
}

func catCount(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catCountColumns); done {
		return res, err
	}
	var query M
	if len(bytes.TrimSpace(body)) > 0 {
		m, err := decodeBody(r, body)
		if err != nil {
			return engine.Response{}, err
		}
		query = m
	}
	res, err := h.c.Count(cr.get("index"), query, cr.engineParams())
	if err != nil {
		return engine.Response{}, err
	}
	count, _ := catNumber(res.Body.(M)["count"])
	row := map[string]any{"count": count}
	catAddTimestamp(row, h.c.Now())
	return catRender(cr, &catTable{cols: catCountColumns, rows: []map[string]any{row}})
}

func catNodes(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catNodesColumns); done {
		return res, err
	}
	fullID, err := cr.boolean("full_id", false)
	if err != nil {
		return engine.Response{}, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	id := node.shortID()
	if fullID {
		id = node.id
	}
	// host statistics (disk, heap, memory, load) are not collected
	row := map[string]any{
		"id": id, "pid": int64(os.Getpid()), "ip": node.ip, "port": node.port, "http_address": node.httpAddress, "version": engine.Version,
		"type": "tar", "build": "osmem", "heap.current": catBytes(0), "heap.percent": 0, "heap.max": catBytes(0),
		"ram.current": catBytes(0), "ram.percent": 0, "ram.max": catBytes(0), "file_desc.current": int64(-1), "file_desc.percent": 0,
		"file_desc.max": int64(-1), "cpu": 0, "load_1m": "0.00", "load_5m": "0.00", "load_15m": "0.00",
		"uptime":    catTime(time.Since(catStarted).Milliseconds() * int64(time.Millisecond)),
		"node.role": "dimr", "node.roles": "cluster_manager,data,ingest,remote_cluster_client", "cluster_manager": "*", "name": node.name,
	}
	catFillZeros(row, catNodesColumns, "jdk", "disk.total", "disk.used", "disk.avail", "disk.used_percent")
	return catRender(cr, &catTable{cols: catNodesColumns, rows: []map[string]any{row}})
}

func catMaster(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catMasterColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	row := map[string]any{"id": node.id, "host": node.host, "ip": node.ip, "node": node.name}
	return catRender(cr, &catTable{cols: catMasterColumns, rows: []map[string]any{row}})
}

func catNodeattrs(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catNodeattrsColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	node := h.catNode()
	// OpenSearch nodes always carry the shard indexing pressure attribute
	row := map[string]any{"node": node.name, "id": node.shortID(), "pid": int64(os.Getpid()), "host": node.host, "ip": node.ip, "port": node.port,
		"attr": "shard_indexing_pressure_enabled", "value": "true"}
	return catRender(cr, &catTable{cols: catNodeattrsColumns, rows: []map[string]any{row}})
}

var catTaskIDs atomic.Int64

// catTaskResourceStats is the resource usage the list tasks action reports
// for itself with ?detailed.
const catTaskResourceStats = "{\n  \"average\" : {\n    \"cpu_time_in_nanos\" : 0,\n    \"memory_in_bytes\" : 0\n  },\n" +
	"  \"total\" : {\n    \"cpu_time_in_nanos\" : 0,\n    \"memory_in_bytes\" : 0\n  },\n" +
	"  \"min\" : {\n    \"cpu_time_in_nanos\" : 0,\n    \"memory_in_bytes\" : 0\n  },\n" +
	"  \"max\" : {\n    \"cpu_time_in_nanos\" : 0,\n    \"memory_in_bytes\" : 0\n  },\n" +
	"  \"thread_info\" : {\n    \"thread_executions\" : 0,\n    \"active_threads\" : 0\n  }\n}"

func catTasks(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	detailed, err := cr.boolean("detailed", false)
	if err != nil {
		return engine.Response{}, err
	}
	cols := catTasksColumns
	if detailed {
		cols = append(append([]catColumn{}, catTasksColumns...), catColumns("description;alias:desc;desc:task action", "resource_stats;default:false;desc:resource consumption info of the task")...)
	}
	if res, done, err := catHelp(cr, cols, "detailed"); done {
		return res, err
	}
	node, now := h.catNode(), h.c.Now()
	opaque := r.Header.Get("X-Opaque-Id")
	if opaque == "" {
		opaque = "-"
	}
	// the only running task is this list tasks request and its node level child
	parent := catTaskIDs.Add(2) - 1
	var rows []map[string]any
	for i, action := range []string{"cluster:monitor/tasks/lists", "cluster:monitor/tasks/lists[n]"} {
		id := parent + int64(i)
		row := map[string]any{
			"id": id, "action": action, "task_id": node.id + ":" + strconv.FormatInt(id, 10), "parent_task_id": "-", "type": "transport",
			"start_time": now.UnixMilli(), "running_time_ns": int64(0), "running_time": catTime(0), "node_id": node.shortID(),
			"ip": node.ip, "port": node.port, "node": node.name, "version": engine.Version, "x_opaque_id": opaque,
		}
		if i == 1 {
			row["parent_task_id"], row["type"] = node.id+":"+strconv.FormatInt(parent, 10), "direct"
		}
		if detailed {
			row["description"], row["resource_stats"] = "", catTaskResourceStats
		}
		catAddTimestamp(row, now)
		delete(row, "epoch")
		rows = append(rows, row)
	}
	var filtered []map[string]any
	actions := catSplitComma(cr.get("actions"))
	parentFilter := cr.get("parent_task_id")
	for _, row := range rows {
		ok := len(actions) == 0
		for _, a := range actions {
			ok = ok || globMatch(a, row["action"].(string))
		}
		if nodes := cr.get("nodes"); nodes != "" && !node.matches(nodes) {
			ok = false
		}
		if parentFilter != "" && parentFilter != "-" && row["parent_task_id"] != parentFilter {
			ok = false
		}
		if ok {
			filtered = append(filtered, row)
		}
	}
	return catRender(cr, &catTable{cols: cols, rows: filtered})
}

// catEmpty serves tables that osmem never has rows for (no plugins,
// pending cluster tasks, loaded fielddata or snapshot repositories).
func catEmpty(cols []catColumn) handlerFunc {
	return func(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
		cr := newCatRequest(r, v)
		if res, done, err := catHelp(cr, cols); done {
			return res, err
		}
		if _, err := cr.boolean("local", false); err != nil {
			return engine.Response{}, err
		}
		return catRender(cr, &catTable{cols: cols})
	}
}

func catSnapshots(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catSnapshotsColumns); done {
		return res, err
	}
	repo, ok := cr.param("repository")
	if !ok || repo == "" {
		return engine.Response{}, &engine.Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: 1: repository is missing;"}
	}
	// osmem has no snapshot repositories
	return engine.Response{}, &engine.Error{Status: http.StatusNotFound, Type: "repository_missing_exception", Reason: "[" + repo + "] missing"}
}

func catSegmentReplication(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catSegmentReplicationColumns); done {
		return res, err
	}
	if _, err := cr.boolean("detailed", false); err != nil {
		return engine.Response{}, err
	}
	// indices use document replication, so no shard reports segment replication
	if _, err := h.c.CatIndices(cr.get("index"), cr.engineParams()); err != nil {
		return engine.Response{}, err
	}
	return catRender(cr, &catTable{cols: catSegmentReplicationColumns})
}

func catTemplates(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	cr := newCatRequest(r, v)
	if res, done, err := catHelp(cr, catTemplatesColumns); done {
		return res, err
	}
	if _, err := cr.boolean("local", false); err != nil {
		return engine.Response{}, err
	}
	pattern, filtered := cr.param("name")
	include := func(name string) bool { return !filtered || globMatch(pattern, name) }
	t := &catTable{cols: catTemplatesColumns}
	// legacy templates come first, then composable index templates
	if res, err := h.c.GetLegacyTemplate("", engine.Params{}); err == nil && res.Status == http.StatusOK {
		legacy, _ := res.Body.(M)
		names := make([]string, 0, len(legacy))
		for n := range legacy {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			tm, _ := legacy[n].(M)
			if !include(n) || tm == nil {
				continue
			}
			order, _ := catNumber(tm["order"])
			row := map[string]any{"name": n, "index_patterns": "[" + strings.Join(catStrings(tm["index_patterns"]), ", ") + "]", "order": int(order), "composed_of": ""}
			if version, ok := catNumber(tm["version"]); ok {
				row["version"] = int(version)
			}
			t.rows = append(t.rows, row)
		}
	}
	if res, err := h.c.GetIndexTemplate("", engine.Params{}); err == nil && res.Status == http.StatusOK {
		body, _ := res.Body.(M)
		list, _ := body["index_templates"].([]any)
		for _, entry := range list {
			em, _ := entry.(M)
			name, _ := em["name"].(string)
			it, _ := em["index_template"].(M)
			if em == nil || !include(name) {
				continue
			}
			priority, _ := catNumber(it["priority"])
			row := map[string]any{
				"name": name, "index_patterns": "[" + strings.Join(catStrings(it["index_patterns"]), ", ") + "]",
				"order": priority, "composed_of": "[" + strings.Join(catStrings(it["composed_of"]), ", ") + "]",
			}
			if version, ok := catNumber(it["version"]); ok {
				row["version"] = version
			}
			t.rows = append(t.rows, row)
		}
	}
	return catRender(cr, t)
}

// catListing is the /_cat help text for the cat APIs osmem serves, in
// OpenSearch's order.
const catListing = "=^.^=\n" +
	"/_cat/allocation\n" +
	"/_cat/segment_replication\n" +
	"/_cat/segment_replication/{index}\n" +
	"/_cat/shards\n" +
	"/_cat/shards/{index}\n" +
	"/_cat/cluster_manager\n" +
	"/_cat/nodes\n" +
	"/_cat/tasks\n" +
	"/_cat/indices\n" +
	"/_cat/indices/{index}\n" +
	"/_cat/segments\n" +
	"/_cat/segments/{index}\n" +
	"/_cat/count\n" +
	"/_cat/count/{index}\n" +
	"/_cat/recovery\n" +
	"/_cat/recovery/{index}\n" +
	"/_cat/health\n" +
	"/_cat/pending_tasks\n" +
	"/_cat/aliases\n" +
	"/_cat/aliases/{alias}\n" +
	"/_cat/thread_pool\n" +
	"/_cat/thread_pool/{thread_pools}\n" +
	"/_cat/plugins\n" +
	"/_cat/fielddata\n" +
	"/_cat/fielddata/{fields}\n" +
	"/_cat/nodeattrs\n" +
	"/_cat/repositories\n" +
	"/_cat/snapshots/{repository}\n" +
	"/_cat/templates\n"

func catHelpListing(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	return engine.Response{Status: http.StatusOK, Body: rawText(catListing)}, nil
}

// nodeInfoMetrics are the metrics RestNodesInfoAction accepts in place of a
// node filter.
var nodeInfoMetrics = map[string]bool{"_all": true, "settings": true, "os": true, "process": true, "jvm": true, "thread_pool": true,
	"transport": true, "http": true, "plugins": true, "ingest": true, "aggregations": true, "indices": true, "search_pipelines": true, "info": true, "stats": true}

// nodeFilterMatches reports whether a node filter selects the single node.
func nodeFilterMatches(h *httpHandler, filter string) bool {
	host := h.address()
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	for _, item := range strings.Split(filter, ",") {
		switch {
		case item == "_local" || item == "_all" || item == "*" || item == "_master" || item == "_cluster_manager" || item == "osmem-node" || item == host:
			return true
		case strings.Contains(item, ":"):
			return true
		case engine.WildcardMatch(item, "osmem-node"):
			return true
		}
	}
	return false
}

func nodesInfo(h *httpHandler, r *http.Request, v map[string]string, body []byte) (engine.Response, error) {
	for _, seg := range []string{v["a"], v["b"]} {
		switch seg {
		case "stats", "usage", "hot_threads", "reload_secure_settings":
			// node statistics, usage and hot threads are not modeled
			return engine.Response{}, &engine.Error{Status: 400, Type: "unsupported_operation_exception", Reason: "_nodes/" + seg + " is not supported by osmem"}
		}
	}
	if first := v["a"]; first != "" {
		metricsOnly := true
		for _, item := range strings.Split(first, ",") {
			if !nodeInfoMetrics[item] {
				metricsOnly = false
			}
		}
		if !metricsOnly && !nodeFilterMatches(h, first) {
			return engine.Response{Status: 200, Body: M{"_nodes": M{"total": 0, "successful": 0, "failed": 0}, "cluster_name": h.c.Name, "nodes": M{}}}, nil
		}
	}
	return engine.Response{Status: 200, Body: M{
		"_nodes": M{"total": 1, "successful": 1, "failed": 0}, "cluster_name": h.c.Name,
		"nodes": M{"osmem-node": M{"name": "osmem-node", "transport_address": h.address(), "host": "127.0.0.1", "ip": "127.0.0.1", "version": engine.Version, "build_type": "tar", "roles": []any{"cluster_manager", "data", "ingest"},
			"http": M{"publish_address": h.address(), "bound_address": []any{h.address()}}}},
	}}, nil
}

// engineQuery re-encodes the query string without the parameters OpenSearch
// treats as absent (blank optional booleans, terminate_after=0, sort entries
// with an unknown order), so engine code sees what OpenSearch applies.
func (api *restAPI) engineQuery(p engine.Params) (string, bool) {
	out := engine.Params{}
	changed := false
	for k, v := range p {
		out[k] = v
	}
	for _, rule := range api.rules {
		v, ok := out[rule.name]
		if !ok {
			continue
		}
		switch {
		case rule.optionalBool && strings.TrimSpace(v) == "":
			delete(out, rule.name)
			changed = true
		case rule.zeroIsUnset && v == "0":
			delete(out, rule.name)
			changed = true
		}
	}
	if sort, ok := out["sort"]; ok && api.sortParam {
		var kept []string
		for _, entry := range engine.JavaSplitComma(sort) {
			if i := strings.LastIndexByte(entry, ':'); i >= 0 {
				if order := entry[i+1:]; order != "asc" && order != "desc" {
					changed = true
					continue
				}
			}
			kept = append(kept, entry)
		}
		out["sort"] = strings.Join(kept, ",")
	}
	if !changed {
		return "", false
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(out[k]))
	}
	return strings.Join(parts, "&"), true
}
