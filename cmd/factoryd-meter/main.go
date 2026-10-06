// Command factoryd-meter is the OpenShell supervisor-middleware service that
// counts each sandbox's model token and cost usage and refuses requests past
// its ceilings.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"buildgate/internal/meter"
)

const reaperInterval = time.Minute

func main() {
	if err := run(os.Args[1:], log.New(os.Stderr, "factoryd-meter: ", log.LstdFlags)); err != nil {
		fmt.Fprintln(os.Stderr, "factoryd-meter:", err)
		os.Exit(1)
	}
}

func run(args []string, logger *log.Logger) error {
	flags := flag.NewFlagSet("factoryd-meter", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:50051", "address to serve gRPC on")
	ledgerRoot := flags.String("ledger-root", "", "directory holding <run>/<sandbox_id>.jsonl ledgers (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *ledgerRoot == "" {
		return fmt.Errorf("-ledger-root is required")
	}
	server, err := meter.NewServer(meter.Config{LedgerRoot: *ledgerRoot, Now: time.Now, Logger: logger})
	if err != nil {
		return err
	}
	// The listener is plaintext and unauthenticated for now: TLS and caller
	// verification arrive with the host services that launch this meter.
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go server.RunReaper(ctx, reaperInterval)
	go func() {
		<-ctx.Done()
		server.Stop()
	}()
	logger.Printf("serving on %s, ledgers under %s", lis.Addr(), *ledgerRoot)
	return server.Serve(lis)
}
