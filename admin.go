package osmem

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shibukawa/osmem/internal/engine"
)

// Management endpoints under /_osmem let non-Go test suites freeze a base
// cluster and create clones served on their own ports:
//
//	POST   /_osmem/base/freeze     freeze the base (implicit at first clone)
//	GET    /_osmem/base            {"frozen": bool, "clones": n}
//	POST   /_osmem/clones          {"id": "...", "url": "http://127.0.0.1:port"}
//	GET    /_osmem/clones          list clones
//	GET    /_osmem/clones/{id}     one clone
//	DELETE /_osmem/clones/{id}     close the clone and its server
//	DELETE /_osmem/clones          close all clones
//
// A frozen cluster rejects writes with 403 osmem_base_frozen; searches and
// other read requests keep working.

type cloneEntry struct {
	id      string
	cluster *Cluster
	server  *Server
	created time.Time
}

type adminState struct {
	frozen atomic.Bool
	mu     sync.Mutex
	clones map[string]*cloneEntry
	seq    int64
}

// Freeze marks the cluster as read-only through HTTP: write requests return
// 403 osmem_base_frozen. Go callers can still write. Creating a clone via
// the management API freezes the base implicitly.
func (c *Cluster) Freeze() { c.admin.frozen.Store(true) }

// Unfreeze allows HTTP writes again.
func (c *Cluster) Unfreeze() { c.admin.frozen.Store(false) }

// Frozen reports whether HTTP writes are rejected.
func (c *Cluster) Frozen() bool { return c.admin.frozen.Load() }

// ManagedClone creates a clone served on its own loopback port and tracks
// it so it can be listed and closed through the management API (or
// CloseManagedClone). The base becomes frozen.
func (c *Cluster) ManagedClone() (id string, srv *Server, clone *Cluster, err error) {
	c.Freeze()
	clone = c.Clone()
	srv, err = clone.Serve()
	if err != nil {
		clone.Close()
		return "", nil, nil, err
	}
	c.admin.mu.Lock()
	c.admin.seq++
	id = "c" + strconv.FormatInt(c.admin.seq, 10)
	c.admin.clones[id] = &cloneEntry{id: id, cluster: clone, server: srv, created: time.Now()}
	c.admin.mu.Unlock()
	return id, srv, clone, nil
}

// CloseManagedClone closes a clone created by ManagedClone.
func (c *Cluster) CloseManagedClone(id string) bool {
	c.admin.mu.Lock()
	e, ok := c.admin.clones[id]
	delete(c.admin.clones, id)
	c.admin.mu.Unlock()
	if !ok {
		return false
	}
	e.server.Close()
	e.cluster.Close()
	return true
}

func (c *Cluster) closeManagedClones() {
	c.admin.mu.Lock()
	entries := c.admin.clones
	c.admin.clones = map[string]*cloneEntry{}
	c.admin.mu.Unlock()
	for _, e := range entries {
		e.server.Close()
		e.cluster.Close()
	}
}

// ManagedClones lists the ids of clones created by ManagedClone.
func (c *Cluster) ManagedClones() []string {
	c.admin.mu.Lock()
	defer c.admin.mu.Unlock()
	ids := make([]string, 0, len(c.admin.clones))
	for id := range c.admin.clones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// rootHandler serves the management API, enforces freezing and delegates
// everything else to the OpenSearch handler.
type rootHandler struct {
	c *Cluster
}

func (h rootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	if path == "_osmem" || strings.HasPrefix(path, "_osmem/") {
		h.serveAdmin(w, r, strings.TrimPrefix(strings.TrimPrefix(path, "_osmem"), "/"))
		return
	}
	if h.c.Frozen() && !isReadOnlyRequest(r.Method, path) {
		writeError(w, r, &engine.Error{Status: http.StatusForbidden, Type: "osmem_base_frozen",
			Reason: "the base cluster is frozen; write to a clone created with POST /_osmem/clones"})
		return
	}
	h.c.osHandler.ServeHTTP(w, r)
}

var readOnlySegments = map[string]bool{
	"_search": true, "_count": true, "_msearch": true, "_mget": true, "_analyze": true, "_explain": true,
	"_validate": true, "_field_caps": true, "_refresh": true, "_flush": true, "_forcemerge": true, "_cache": true,
	"_stats": true, "_mapping": true, "_settings": true, "_alias": true, "_aliases": true, "_cat": true, "_cluster": true, "_nodes": true,
}

// isReadOnlyRequest reports whether a request cannot change cluster state.
func isReadOnlyRequest(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if readOnlySegments[s] {
			// PUT/POST on _mapping, _settings, _alias(es) are writes
			switch s {
			case "_mapping", "_settings", "_alias", "_aliases":
				return false
			}
			return true
		}
	}
	return false
}

func (h rootHandler) serveAdmin(w http.ResponseWriter, r *http.Request, rest string) {
	segs := strings.Split(rest, "/")
	if rest == "" {
		segs = nil
	}
	switch {
	case len(segs) == 0 && r.Method == http.MethodGet:
		writeJSON(w, 200, M{"version": engine.Version, "frozen": h.c.Frozen(), "clones": len(h.c.ManagedClones())})
	case len(segs) == 1 && segs[0] == "base" && r.Method == http.MethodGet:
		writeJSON(w, 200, M{"frozen": h.c.Frozen(), "clones": len(h.c.ManagedClones()), "indices": h.c.Indices()})
	case len(segs) == 2 && segs[0] == "base" && segs[1] == "freeze" && r.Method == http.MethodPost:
		h.c.Freeze()
		writeJSON(w, 200, M{"acknowledged": true, "frozen": true})
	case len(segs) == 2 && segs[0] == "base" && segs[1] == "unfreeze" && r.Method == http.MethodPost:
		h.c.Unfreeze()
		writeJSON(w, 200, M{"acknowledged": true, "frozen": false})
	case len(segs) == 1 && segs[0] == "clones" && r.Method == http.MethodPost:
		id, srv, _, err := h.c.ManagedClone()
		if err != nil {
			writeError(w, r, &engine.Error{Status: 500, Type: "osmem_clone_failed", Reason: err.Error()})
			return
		}
		writeJSON(w, 201, M{"id": id, "url": srv.URL})
	case len(segs) == 1 && segs[0] == "clones" && r.Method == http.MethodGet:
		writeJSON(w, 200, M{"clones": h.cloneList()})
	case len(segs) == 1 && segs[0] == "clones" && r.Method == http.MethodDelete:
		n := len(h.c.ManagedClones())
		h.c.closeManagedClones()
		writeJSON(w, 200, M{"acknowledged": true, "closed": n})
	case len(segs) == 2 && segs[0] == "clones" && r.Method == http.MethodGet:
		for _, e := range h.cloneList() {
			if e["id"] == segs[1] {
				writeJSON(w, 200, e)
				return
			}
		}
		writeError(w, r, &engine.Error{Status: 404, Type: "osmem_clone_not_found", Reason: "no clone [" + segs[1] + "]"})
	case len(segs) == 2 && segs[0] == "clones" && r.Method == http.MethodDelete:
		if !h.c.CloseManagedClone(segs[1]) {
			writeError(w, r, &engine.Error{Status: 404, Type: "osmem_clone_not_found", Reason: "no clone [" + segs[1] + "]"})
			return
		}
		writeJSON(w, 200, M{"acknowledged": true})
	default:
		writeResponse(w, r, engine.Response{Status: 400, Body: M{"error": "no handler found for uri [" + r.URL.RequestURI() + "] and method [" + r.Method + "]"}})
	}
}

func (h rootHandler) cloneList() []M {
	h.c.admin.mu.Lock()
	defer h.c.admin.mu.Unlock()
	out := make([]M, 0, len(h.c.admin.clones))
	for _, e := range h.c.admin.clones {
		out = append(out, M{"id": e.id, "url": e.server.URL, "created": e.created.UTC().Format(time.RFC3339Nano)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
