// Command osmem-server runs an in-memory OpenSearch-compatible server for
// test suites in any language. It prints a JSON line with the base URL when
// ready and exits when stdin closes, so a parent test runner that spawns it
// with a pipe never leaves it behind.
//
//	osmem-server [--addr 127.0.0.1:0] [--seed DIR|FILE]... [--freeze] [--no-ja] [--parent-pid N] [--no-stdin-watch]
//
// Clones for individual tests are created through the management API:
// POST /_osmem/clones returns {"id","url"}; DELETE /_osmem/clones/{id}
// closes one. See the osmem README.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shibukawa/osmem/internal/serve"
)

// version is set by scripts/build-binaries.sh via -ldflags "-X main.version=...".
var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var seeds multiFlag
	addr := flag.String("addr", "127.0.0.1:0", "listen address (port 0 picks a free port)")
	flag.Var(&seeds, "seed", "seed directory or .ndjson file; repeatable, loaded in order")
	freeze := flag.Bool("freeze", false, "freeze the base after seeding (writes only through clones)")
	noJa := flag.Bool("no-ja", false, "disable Japanese analysis (kuromoji falls back to CJK bigrams)")
	parentPID := flag.Int("parent-pid", 0, "exit when this process id disappears")
	noStdinWatch := flag.Bool("no-stdin-watch", false, "do not exit when stdin closes")
	showVersion := flag.Bool("version", false, "print the osmem-server version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("osmem-server %s (OpenSearch API %s)\n", version, serve.APIVersion)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := serve.Run(ctx, serve.Options{
		Addr:       *addr,
		Seeds:      seeds,
		Japanese:   !*noJa,
		Freeze:     *freeze,
		StdinWatch: !*noStdinWatch,
		ParentPID:  *parentPID,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "osmem-server:", err)
		os.Exit(1)
	}
}
