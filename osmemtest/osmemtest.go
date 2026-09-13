// Package osmemtest contains testing.TB-aware helpers for osmem: clones and
// servers whose lifetime is bound to a test through t.Cleanup.
//
//	var base *osmem.Cluster
//
//	func TestMain(m *testing.M) {
//	    base = osmem.New()
//	    defer base.Close()
//	    if err := base.LoadSeed("testdata/seed"); err != nil {
//	        log.Fatal(err)
//	    }
//	    m.Run()
//	}
//
//	func TestSearch(t *testing.T) {
//	    c, srv := osmemtest.CloneAndServe(t, base)
//	    client := newClient(srv.URL)
//	    ...
//	}
//
// There is intentionally no helper that takes *testing.M: TestMain stays
// plain Go, so osmem composes with other in-memory fakes (pgmem, ...) that
// set themselves up in the same function.
package osmemtest

import (
	"testing"

	"github.com/shibukawa/osmem"
)

// Clone returns a clone of base that is closed when the test ends.
func Clone(t testing.TB, base *osmem.Cluster) *osmem.Cluster {
	t.Helper()
	c := base.Clone()
	t.Cleanup(c.Close)
	return c
}

// Serve starts an HTTP server for c on a loopback port and stops it when
// the test ends.
func Serve(t testing.TB, c *osmem.Cluster) *osmem.Server {
	t.Helper()
	srv, err := c.Serve()
	if err != nil {
		t.Fatalf("osmemtest: serve: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// CloneAndServe clones base and serves the clone; both are released when
// the test ends. srv.URL is the address to give to an OpenSearch client.
func CloneAndServe(t testing.TB, base *osmem.Cluster) (*osmem.Cluster, *osmem.Server) {
	t.Helper()
	c := Clone(t, base)
	return c, Serve(t, c)
}

// New creates an empty cluster that is closed when the test ends.
func New(t testing.TB, opts ...osmem.Option) *osmem.Cluster {
	t.Helper()
	c := osmem.New(opts...)
	t.Cleanup(c.Close)
	return c
}
