package osmem

import (
	"net/http"
	"strings"

	"github.com/shibukawa/osmem/internal/engine"
)

// serverlessRoutes lists the routes Amazon OpenSearch Serverless serves, as
// "METHODS /pattern" with every path variable written as *. It follows the
// operation table of "Supported operations and plugins in Amazon OpenSearch
// Serverless"; read APIs listed for one of GET/POST accept both, as on
// OpenSearch. Every other route answers 404 with an empty body there.
var serverlessRoutes = buildServerlessRoutes(
	// aoss:CreateIndex, DescribeIndex, DeleteIndex
	"GET,HEAD,PUT,DELETE /*",
	"GET /*/_mapping", "GET /*/_mappings", "GET /_mapping", "GET /_mappings",
	"GET /*/_settings", "GET /*/_settings/*", "GET /_settings", "GET /_settings/*",
	"GET /_cat/indices", "GET /_cat/indices/*",
	"GET /_resolve/index/*",
	// aoss:UpdateIndex
	"PUT,POST /*/_mapping", "PUT,POST /*/_mappings", "PUT,POST /_mapping",
	"PUT,POST /*/_settings", "PUT,POST /_settings",
	// aoss:WriteDocument (document IDs only for search collections, see
	// engine.Cluster.rejectsDocumentIDs)
	"POST /*/_doc", "PUT,POST,DELETE /*/_doc/*",
	"PUT,POST /*/_create/*", "POST /*/_update/*",
	"PUT,POST /_bulk", "PUT,POST /*/_bulk",
	// aoss:ReadDocument
	"GET,HEAD /*/_doc/*", "GET,HEAD /*/_source/*",
	"GET,POST /_analyze", "GET,POST /*/_analyze",
	"GET,POST /*/_explain/*",
	"GET,POST /_mget", "GET,POST /*/_mget",
	"GET,POST /_msearch", "GET,POST /*/_msearch",
	"GET,POST /_search", "GET,POST /*/_search",
	"GET,POST /_count", "GET,POST /*/_count",
	"GET,POST /_field_caps", "GET,POST /*/_field_caps",
	"GET,POST /_validate/query", "GET,POST /*/_validate/query",
	"POST /*/_search/point_in_time", "DELETE /_search/point_in_time",
	"GET,DELETE /_search/point_in_time/_all",
	// aoss:*CollectionItems (aliases and templates)
	"POST /_aliases",
	"GET /_alias", "GET,HEAD /_alias/*", "GET,HEAD /*/_alias", "GET,HEAD /*/_alias/*",
	"PUT,POST,DELETE /*/_alias/*", "PUT,POST,DELETE /*/_aliases/*",
	"GET /_cat/aliases", "GET /_cat/aliases/*", "GET /_cat/templates", "GET /_cat/templates/*",
	"GET /_component_template", "GET,HEAD,PUT,POST,DELETE /_component_template/*",
	"GET /_index_template", "GET,HEAD,PUT,POST,DELETE /_index_template/*",
)

func buildServerlessRoutes(specs ...string) map[string]bool {
	out := map[string]bool{}
	for _, spec := range specs {
		methods, pattern, _ := strings.Cut(spec, " ")
		for _, m := range strings.Split(methods, ",") {
			out[m+" "+pattern] = true
		}
	}
	return out
}

// serverlessRouteKey normalizes a route pattern to its serverlessRoutes key.
func serverlessRouteKey(method string, pattern []string) string {
	var sb strings.Builder
	sb.WriteString(method)
	sb.WriteString(" ")
	n := 0
	for _, seg := range pattern {
		if seg == "" {
			continue
		}
		sb.WriteString("/")
		if strings.HasPrefix(seg, "{") {
			seg = "*"
		}
		sb.WriteString(seg)
		n++
	}
	if n == 0 {
		sb.WriteString("/")
	}
	return sb.String()
}

// serverlessReject answers a request Serverless does not accept, or returns
// false: routes outside serverlessRoutes are 404 with an empty body, and a
// write asking for refresh=true or wait_for is a status_exception.
func serverlessReject(w http.ResponseWriter, r *http.Request, rt *route, p engine.Params) bool {
	if !serverlessRoutes[serverlessRouteKey(r.Method, rt.pattern)] {
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead || !p.Has("refresh") {
		return false
	}
	policy := p.Get("refresh")
	switch policy {
	case "", "true":
		policy = "true"
	case "wait_for":
	default:
		return false
	}
	writeError(w, r, &engine.Error{Status: http.StatusBadRequest, Type: "status_exception", Reason: policy + " refresh policy is not supported."})
	return true
}
