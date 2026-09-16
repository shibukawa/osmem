// Package osmem is an in-memory, OpenSearch-compatible fake for Go tests.
//
// A Cluster holds indices, mappings, documents and aliases. Its Handler
// speaks the OpenSearch REST API (index/document CRUD, bulk, search with
// the query DSL, aggregations, scroll, aliases, templates, ...) so any
// OpenSearch or Elasticsearch HTTP client can talk to it. Clone creates an
// independent copy of the cluster in O(1); indices are shared until one
// side writes to them, so a seeded base cluster can be cloned per test and
// thrown away.
package osmem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/shibukawa/osmem/internal/engine"
)

// Version is the OpenSearch version osmem reports.
const Version = engine.Version

// Cluster is an in-memory OpenSearch-compatible cluster.
type Cluster struct {
	eng       *engine.Cluster
	osHandler http.Handler // OpenSearch REST API
	handler   http.Handler // management API + freeze check + osHandler
	admin     adminState
}

// Option configures a cluster.
type Option func(*engine.Cluster)

// WithClock sets the clock used for "now" in date math and creation dates.
func WithClock(now func() time.Time) Option {
	return func(c *engine.Cluster) { c.Now = now }
}

// WithWarnings receives a message whenever an unsupported feature (for
// example an analyzer without a bleve equivalent) is ignored.
func WithWarnings(fn func(msg string)) Option {
	return func(c *engine.Cluster) { c.Warn = fn }
}

// WithClusterName sets the reported cluster name.
func WithClusterName(name string) Option {
	return func(c *engine.Cluster) { c.Name = name }
}

// New creates an empty cluster.
func New(opts ...Option) *Cluster {
	eng := engine.New()
	for _, o := range opts {
		o(eng)
	}
	return wrap(eng)
}

func wrap(eng *engine.Cluster) *Cluster {
	c := &Cluster{eng: eng, osHandler: newHTTPHandler(eng)}
	c.admin.clones = map[string]*cloneEntry{}
	c.handler = rootHandler{c: c}
	return c
}

// Clone returns an independent copy of the cluster. The copy shares index
// data with the original until either of them writes, so cloning a seeded
// cluster is cheap. Close the clone when done.
func (c *Cluster) Clone() *Cluster {
	return wrap(c.eng.Clone())
}

// Close releases the cluster's resources. Clones made with Clone stay
// usable; clones created through the management API are closed too.
func (c *Cluster) Close() {
	c.closeManagedClones()
	c.eng.Close()
}

// Handler returns the HTTP handler: the OpenSearch REST API plus the
// management endpoints under /_osmem (see admin.go).
func (c *Cluster) Handler() http.Handler {
	return c.handler
}

// Server is a running HTTP server for a cluster.
type Server struct {
	// URL is the base URL ("http://127.0.0.1:port") to give to clients.
	URL string
	srv *http.Server
}

// Serve starts an HTTP server on a random loopback port. The cluster keeps
// working after the server is closed.
func (c *Cluster) Serve() (*Server, error) {
	return c.ServeAddr("127.0.0.1:0")
}

// ServeAddr starts an HTTP server on the given address ("host:port"; port
// 0 picks a free one).
func (c *Cluster) ServeAddr(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	// header and idle timeouts keep a stalled or abandoned connection from
	// pinning a goroutine for the life of the server
	srv := &http.Server{Handler: c.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	c.eng.HTTPAddress = ln.Addr().String()
	s := &Server{URL: "http://" + ln.Addr().String(), srv: srv}
	go func() { _ = srv.Serve(ln) }()
	return s, nil
}

// MustServe is Serve that panics on error.
func (c *Cluster) MustServe() *Server {
	s, err := c.Serve()
	if err != nil {
		panic(err)
	}
	return s
}

// Close stops the server.
func (s *Server) Close() {
	_ = s.srv.Close()
}

// Response is the result of an in-process request.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// IsError reports whether the status code is 4xx or 5xx.
func (r *Response) IsError() bool { return r.StatusCode >= 400 }

// Err returns an error describing the response when it is an error response.
func (r *Response) Err() error {
	if !r.IsError() {
		return nil
	}
	var e struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(r.Body, &e) == nil && e.Error.Type != "" {
		return fmt.Errorf("osmem: %s: %s (status %d)", e.Error.Type, e.Error.Reason, r.StatusCode)
	}
	return fmt.Errorf("osmem: status %d: %s", r.StatusCode, strings.TrimSpace(string(r.Body)))
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) error {
	return json.Unmarshal(r.Body, v)
}

// Do performs a request against the cluster without going through the
// network. path includes the query string ("/products/_search?size=1").
// body may be nil, a string, []byte, json.RawMessage, io.Reader or any value
// that is marshalled as JSON.
func (c *Cluster) Do(method, path string, body any) (*Response, error) {
	rd, err := bodyReader(body)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.ContainsAny(path, " \t\r\n") {
		// httptest.NewRequest panics on these; the helpers escape their
		// segments, callers of Do escape their own
		return nil, fmt.Errorf("osmem: invalid request path %q (escape segments with url.PathEscape)", path)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return &Response{StatusCode: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}, nil
}

func bodyReader(body any) (io.Reader, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case string:
		return strings.NewReader(b), nil
	case []byte:
		return bytes.NewReader(b), nil
	case json.RawMessage:
		return bytes.NewReader(b), nil
	case io.Reader:
		return b, nil
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(data), nil
	}
}

// CreateIndex creates an index. body is the create-index body (settings,
// mappings, aliases) in any form accepted by Do; nil creates an index with
// default settings.
func (c *Cluster) CreateIndex(name string, body any) error {
	res, err := c.Do(http.MethodPut, "/"+url.PathEscape(name), body)
	if err != nil {
		return err
	}
	return res.Err()
}

// DeleteIndex deletes indices matching the expression.
func (c *Cluster) DeleteIndex(expr string) error {
	res, err := c.Do(http.MethodDelete, "/"+url.PathEscape(expr), nil)
	if err != nil {
		return err
	}
	return res.Err()
}

// Index stores a document (creating the index if needed). An empty id
// generates one.
func (c *Cluster) Index(index, id string, doc any) error {
	var res *Response
	var err error
	if id == "" {
		res, err = c.Do(http.MethodPost, "/"+url.PathEscape(index)+"/_doc", doc)
	} else {
		res, err = c.Do(http.MethodPut, "/"+url.PathEscape(index)+"/_doc/"+url.PathEscape(id), doc)
	}
	if err != nil {
		return err
	}
	return res.Err()
}

// Get returns a document's source, or false when it does not exist.
func (c *Cluster) Get(index, id string, v any) (bool, error) {
	res, err := c.Do(http.MethodGet, "/"+url.PathEscape(index)+"/_source/"+url.PathEscape(id), nil)
	if err != nil {
		return false, err
	}
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err := res.Err(); err != nil {
		return false, err
	}
	if v != nil {
		if err := res.JSON(v); err != nil {
			return true, err
		}
	}
	return true, nil
}

// BulkError reports failed bulk items.
type BulkError struct {
	Items []BulkItemError
}

// BulkItemError is one failed bulk item.
type BulkItemError struct {
	Action string
	Index  string
	ID     string
	Status int
	Type   string
	Reason string
}

func (e *BulkError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "osmem: bulk: %d item(s) failed", len(e.Items))
	for i, it := range e.Items {
		if i >= 5 {
			sb.WriteString("; ...")
			break
		}
		fmt.Fprintf(&sb, "; %s %s/%s: %s: %s", it.Action, it.Index, it.ID, it.Type, it.Reason)
	}
	return sb.String()
}

// Bulk loads newline-delimited bulk actions (the _bulk body format). It
// returns a *BulkError when any item failed.
func (c *Cluster) Bulk(ndjson io.Reader) error {
	res, err := c.Do(http.MethodPost, "/_bulk", ndjson)
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}
	return bulkResponseError(res)
}

// bulkResponseError converts failed items of a bulk response into a
// *BulkError.
func bulkResponseError(res *Response) error {
	var out struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Index  string `json:"_index"`
			ID     string `json:"_id"`
			Status int    `json:"status"`
			Error  *struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"items"`
	}
	if err := res.JSON(&out); err != nil {
		return err
	}
	if !out.Errors {
		return nil
	}
	be := &BulkError{}
	for _, item := range out.Items {
		for action, it := range item {
			if it.Error != nil {
				be.Items = append(be.Items, BulkItemError{Action: action, Index: it.Index, ID: it.ID, Status: it.Status, Type: it.Error.Type, Reason: it.Error.Reason})
			}
		}
	}
	return be
}

// BulkString is Bulk for an in-memory string.
func (c *Cluster) BulkString(ndjson string) error {
	return c.Bulk(strings.NewReader(ndjson))
}

// Search runs a search request body against an index expression and
// decodes the response into v (a struct or map).
func (c *Cluster) Search(index string, body any, v any) error {
	res, err := c.Do(http.MethodPost, "/"+index+"/_search", body)
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return res.JSON(v)
}

// Count returns the number of documents matching an optional query body.
func (c *Cluster) Count(index string, body any) (int, error) {
	res, err := c.Do(http.MethodPost, "/"+index+"/_count", body)
	if err != nil {
		return 0, err
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := res.JSON(&out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// Indices returns the names of all indices.
func (c *Cluster) Indices() []string {
	return c.eng.Indices()
}

// Warnings returns the warnings recorded while building an index's
// analyzers (unsupported filters, analyzers replaced by approximations).
func (c *Cluster) Warnings(index string) []string {
	return c.eng.Warnings(index)
}
