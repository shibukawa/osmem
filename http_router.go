package osmem

import (
	"net/url"
	"strings"
)

// Request routing follows OpenSearch's RestController and PathTrie. Routes
// are stored in a trie whose "{name}" segments are wildcard nodes; a request
// path is tried in four matching modes (explicit nodes only, a wildcard as the
// first segment, a wildcard as the last segment, wildcards anywhere). The
// first mode whose node handles the method wins. When a mode finds a node
// that does not handle the method and no other mode does either, the
// request fails with 405 listing the methods of every node the path matches;
// when no mode finds a node at all there is no handler (400).

type trieMode int

const (
	modeExplicitOnly trieMode = iota
	modeWildcardRoot
	modeWildcardLeaf
	modeWildcardNodes
)

// methodHandlers are the routes registered for one trie node.
type methodHandlers struct {
	pattern string
	methods map[string]*route
}

type trieNode struct {
	children map[string]*trieNode
	wildcard *trieNode
	handlers *methodHandlers
}

type router struct {
	root         trieNode
	rootHandlers *methodHandlers
}

func newRouter(routes []route) *router {
	rt := &router{}
	for i := range routes {
		rt.insert(&routes[i])
	}
	// OpenSearch endpoints osmem does not implement still own their path:
	// without a node there, "/_tasks" would reach the "/{index}" routes.
	for _, name := range unimplementedRootEndpoints {
		if rt.root.children == nil {
			rt.root.children = map[string]*trieNode{}
		}
		node := rt.root.children[name]
		if node == nil {
			node = &trieNode{}
			rt.root.children[name] = node
		}
		if node.handlers == nil {
			node.handlers = &methodHandlers{pattern: "/" + name, methods: map[string]*route{}}
		}
	}
	return rt
}

// unimplementedRootEndpoints are single-segment OpenSearch 3.8 endpoints
// (rest-api-spec) that osmem has no route for; they answer "no handler found".
var unimplementedRootEndpoints = []string{"_component_template", "_dangling", "_data_stream", "_mtermvectors", "_rank_eval",
	"_recovery", "_script_context", "_script_language", "_search_shards", "_segments", "_shard_stores", "_snapshot", "_tasks", "_upgrade"}

func routePattern(segs []string) string { return "/" + strings.Join(segs, "/") }

func isNamedWildcard(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

func (rt *router) insert(r *route) {
	if len(r.pattern) == 0 {
		rt.rootHandlers = addRouteMethods(rt.rootHandlers, "/", r)
		return
	}
	node := &rt.root
	for _, seg := range r.pattern {
		if isNamedWildcard(seg) {
			if node.wildcard == nil {
				node.wildcard = &trieNode{}
			}
			node = node.wildcard
			continue
		}
		if node.children == nil {
			node.children = map[string]*trieNode{}
		}
		child := node.children[seg]
		if child == nil {
			child = &trieNode{}
			node.children[seg] = child
		}
		node = child
	}
	node.handlers = addRouteMethods(node.handlers, routePattern(r.pattern), r)
}

// addRouteMethods registers the methods of r; the first route registered for
// a method keeps it.
func addRouteMethods(h *methodHandlers, pattern string, r *route) *methodHandlers {
	if h == nil {
		h = &methodHandlers{pattern: pattern, methods: map[string]*route{}}
	}
	for m := range r.methods {
		if _, exists := h.methods[m]; !exists {
			h.methods[m] = r
		}
	}
	return h
}

// javaSplitPath is Java's String.split("/"): trailing empty tokens are dropped.
func javaSplitPath(path string) []string {
	parts := strings.Split(path, "/")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func (rt *router) retrieve(path string, mode trieMode) *methodHandlers {
	if path == "" {
		return rt.rootHandlers
	}
	tokens := javaSplitPath(path)
	if len(tokens) == 0 {
		return rt.rootHandlers
	}
	index := 0
	if tokens[0] == "" {
		index = 1
	}
	return rt.root.retrieve(tokens, index, mode)
}

// retrieve is PathTrie.TrieNode.retrieve.
func (n *trieNode) retrieve(path []string, index int, mode trieMode) *methodHandlers {
	if index >= len(path) {
		return nil
	}
	token := path[index]
	node := n.children[token]
	usedWildcard := false
	if node == nil {
		switch {
		case mode == modeWildcardNodes:
			node = n.wildcard
		case mode == modeWildcardRoot && index == 1:
			node = n.wildcard
		case mode == modeWildcardLeaf && index+1 == len(path):
			node = n.wildcard
		}
		if node == nil {
			return nil
		}
		usedWildcard = true
	} else {
		switch {
		case index+1 == len(path) && node.handlers == nil && n.wildcard != nil && mode != modeExplicitOnly && mode != modeWildcardRoot:
			node, usedWildcard = n.wildcard, true
		case index == 1 && node.handlers == nil && n.wildcard != nil && mode == modeWildcardRoot:
			node, usedWildcard = n.wildcard, true
		default:
			usedWildcard = token == "*"
		}
	}
	if index == len(path)-1 {
		return node.handlers
	}
	value := node.retrieve(path, index+1, mode)
	if value == nil && !usedWildcard && mode != modeExplicitOnly && n.wildcard != nil {
		value = n.wildcard.retrieve(path, index+1, mode)
	}
	return value
}

type dispatchOutcome int

const (
	dispatchFound dispatchOutcome = iota
	dispatchMethodNotAllowed
	dispatchOptions
	dispatchNoHandler
)

// restMethods are the methods OpenSearch's RestRequest knows.
var restMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "OPTIONS": true, "HEAD": true, "PATCH": true, "TRACE": true, "CONNECT": true}

// dispatch is RestController.tryAllHandlers: it returns the matched route
// with its path variables, or the allowed methods for a 405 or OPTIONS
// response.
func (rt *router) dispatch(method, rawPath string) (r *route, pattern string, vars map[string]string, allowed []string, outcome dispatchOutcome) {
	for mode := modeExplicitOnly; mode <= modeWildcardNodes; mode++ {
		handlers := rt.retrieve(rawPath, mode)
		if handlers != nil {
			if found := handlers.methods[method]; found != nil {
				return found, handlers.pattern, bindPathVars(found.pattern, rawPath), nil, dispatchFound
			}
		}
		valid := rt.validMethods(rawPath)
		if contains(valid, method) {
			continue
		}
		if method == "OPTIONS" {
			return nil, "", nil, valid, dispatchOptions
		}
		if len(valid) > 0 {
			return nil, "", nil, valid, dispatchMethodNotAllowed
		}
	}
	return nil, "", nil, nil, dispatchNoHandler
}

// validMethods is RestController.getValidHandlerMethodSet, in the order
// OpenSearch prints the set.
func (rt *router) validMethods(rawPath string) []string {
	set := map[string]bool{}
	primary := ""
	for mode := modeExplicitOnly; mode <= modeWildcardNodes; mode++ {
		handlers := rt.retrieve(rawPath, mode)
		if handlers == nil {
			continue
		}
		if primary == "" {
			primary = handlers.pattern
		}
		for m := range handlers.methods {
			set[m] = true
		}
	}
	return orderMethods(set, primary)
}

// OpenSearch prints the allowed methods from a HashSet of the method enum:
// HEAD, DELETE and PUT come first, GET and POST share a bucket and keep the
// order in which their handlers were registered.
var postRegisteredBeforeGet = map[string]bool{
	"/_index_template/{name}":     true,
	"/_component_template/{name}": true,
	"/{index}/_doc/{id}":          true,
	"/{index}/_mapping":           true,
}

func orderMethods(set map[string]bool, pattern string) []string {
	order := []string{"HEAD", "DELETE", "PUT", "GET", "POST", "OPTIONS", "PATCH", "TRACE", "CONNECT"}
	if postRegisteredBeforeGet[pattern] {
		order[3], order[4] = "POST", "GET"
	}
	var out []string
	for _, m := range order {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// bindPathVars maps the named wildcards of a route pattern to the decoded
// segments of the raw path.
func bindPathVars(pattern []string, rawPath string) map[string]string {
	vars := map[string]string{}
	tokens := javaSplitPath(rawPath)
	if len(tokens) > 0 && tokens[0] == "" {
		tokens = tokens[1:]
	}
	for i, seg := range pattern {
		if i >= len(tokens) || !isNamedWildcard(seg) {
			continue
		}
		value := tokens[i]
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		vars[strings.Trim(seg, "{}")] = value
	}
	return vars
}
