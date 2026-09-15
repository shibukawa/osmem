package osmem

import (
	"context"
	"net/http"

	"github.com/shibukawa/osmem/internal/engine"
)

// Parse errors report where in the request body OpenSearch's parser failed
// ("[1:42] [terms] unknown field [foo]", or line and col metadata). The
// engine names the failing token through the decoded body, so handlers
// decode bodies with decodeBody, which remembers the raw bytes of every
// decoded value, and run locates the errors of a handler in them.

type bodyTraceKey struct{}

// bodyTrace lists the bodies a handler decoded.
type bodyTrace struct {
	docs []tracedBody
}

type tracedBody struct {
	data []byte
	root engine.M
}

// run calls the handler of a route and adds body locations to its error.
func (h *httpHandler) run(rt *route, r *http.Request, vars map[string]string, body []byte) (engine.Response, error) {
	tr := &bodyTrace{}
	r = r.WithContext(context.WithValue(r.Context(), bodyTraceKey{}, tr))
	res, err := rt.fn(h, r, vars, body)
	if err != nil {
		for _, doc := range tr.docs {
			engine.LocateError(err, doc.data, doc.root)
		}
	}
	return res, err
}

// traceBody records a body decoded for the request.
func traceBody(r *http.Request, data []byte, root engine.M) {
	if tr, ok := r.Context().Value(bodyTraceKey{}).(*bodyTrace); ok && root != nil {
		tr.docs = append(tr.docs, tracedBody{data: data, root: root})
	}
}

// decodeBody parses a JSON object body of the request.
func decodeBody(r *http.Request, body []byte) (M, error) {
	m, err := engine.DecodeObject(body)
	if err == nil {
		traceBody(r, body, m)
	}
	return m, err
}
