// Package serve implements the osmem-server command: a base cluster seeded
// from files, served over HTTP with the management API for clones.
package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/shibukawa/osmem"
	"github.com/shibukawa/osmem/ja"
)

// APIVersion is the OpenSearch version reported by the server.
const APIVersion = osmem.Version

// Options configure a server run.
type Options struct {
	Addr     string   // listen address, default 127.0.0.1:0
	Seeds    []string // seed directories or .ndjson files, loaded in order
	Japanese bool     // enable kuromoji via kagome (default true in main)
	Freeze   bool     // freeze the base right after seeding
	// StdinWatch exits when Stdin reaches EOF, so the server dies with a
	// parent process that piped its stdin.
	StdinWatch bool
	Stdin      io.Reader
	ParentPID  int // exit when this process disappears (0 = disabled)
	Stdout     io.Writer
	Stderr     io.Writer
	// Ready is called with the base URL once the server listens (tests).
	Ready func(url string)
}

// Ready is the JSON line printed on stdout when the server is listening.
type Ready struct {
	URL      string   `json:"url"`
	PID      int      `json:"pid"`
	Version  string   `json:"version"`
	Japanese bool     `json:"japanese"`
	Indices  []string `json:"indices"`
}

// Run starts the server and blocks until ctx is cancelled, stdin closes
// (StdinWatch) or the parent process exits (ParentPID).
func Run(ctx context.Context, opts Options) error {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	ja.SetEnabled(opts.Japanese)
	c := osmem.New(osmem.WithWarnings(func(msg string) { fmt.Fprintln(opts.Stderr, "osmem-server: warning:", msg) }))
	defer c.Close()
	for _, seed := range opts.Seeds {
		if err := c.LoadSeed(seed); err != nil {
			return err
		}
	}
	if opts.Freeze {
		c.Freeze()
	}
	srv, err := c.ServeAddr(opts.Addr)
	if err != nil {
		return err
	}
	defer srv.Close()
	ready := Ready{URL: srv.URL, PID: os.Getpid(), Version: osmem.Version, Japanese: opts.Japanese, Indices: c.Indices()}
	if ready.Indices == nil {
		ready.Indices = []string{}
	}
	line, _ := json.Marshal(ready)
	fmt.Fprintln(opts.Stdout, string(line))
	if opts.Ready != nil {
		opts.Ready(srv.URL)
	}

	done := make(chan string, 2)
	if opts.StdinWatch {
		go func() {
			r := bufio.NewReader(opts.Stdin)
			for {
				if _, err := r.ReadByte(); err != nil {
					done <- "stdin closed"
					return
				}
			}
		}()
	}
	if opts.ParentPID > 0 {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for range t.C {
				if !processAlive(opts.ParentPID) {
					done <- "parent process exited"
					return
				}
			}
		}()
	}
	select {
	case <-ctx.Done():
	case reason := <-done:
		fmt.Fprintln(opts.Stderr, "osmem-server: exiting:", reason)
	}
	return nil
}
