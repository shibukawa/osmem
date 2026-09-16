package engine

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"
)

// Custom routing values of stored documents. Documents are immutable and
// shared between index copies, so the routing is keyed by the document
// (weakly: the entry goes away with the document).
var docRoutings = struct {
	mu sync.RWMutex
	m  map[weak.Pointer[Doc]]string
	n  atomic.Int64
}{m: map[weak.Pointer[Doc]]string{}}

func setDocRouting(d *Doc, routing string) {
	if d == nil || routing == "" {
		return
	}
	key := weak.Make(d)
	docRoutings.mu.Lock()
	docRoutings.m[key] = routing
	docRoutings.n.Store(int64(len(docRoutings.m)))
	docRoutings.mu.Unlock()
	runtime.AddCleanup(d, func(k weak.Pointer[Doc]) {
		docRoutings.mu.Lock()
		delete(docRoutings.m, k)
		docRoutings.n.Store(int64(len(docRoutings.m)))
		docRoutings.mu.Unlock()
	}, key)
}

// storedFieldOutput renders the values of a stored field: dates in the
// field's format (UTC), geo points as "lat, lon", ip addresses normalized,
// numbers as the field's Java type.
func storedFieldOutput(f *Field, vals []any) []any {
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		switch t := v.(type) {
		case time.Time:
			out = append(out, dateFormatFor(f, "").Format(t.UTC()))
		case float64:
			if f != nil && f.isNumeric() {
				out = append(out, sourceNumberOutput(f, t))
			} else {
				out = append(out, Double(t))
			}
		case exactInt:
			out = append(out, t.output(f))
		case [2]float64:
			out = append(out, javaNumberString(t[0], 64)+", "+javaNumberString(t[1], 64))
		case string:
			if f != nil && f.Type == TypeIP {
				if ip := net.ParseIP(t); ip != nil {
					if v4 := ip.To4(); v4 != nil {
						t = v4.String()
					} else {
						t = ip.String()
					}
				}
			}
			out = append(out, t)
		default:
			out = append(out, v)
		}
	}
	return out
}

// docRouting returns the custom routing a document was indexed with.
func docRouting(d *Doc) string {
	if d == nil || docRoutings.n.Load() == 0 {
		return ""
	}
	docRoutings.mu.RLock()
	defer docRoutings.mu.RUnlock()
	return docRoutings.m[weak.Make(d)]
}
