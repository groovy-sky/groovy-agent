// Command coreutils-mcp serves the read-only coreutils tool set using the
// Model Context Protocol. It defaults to newline-delimited JSON-RPC over
// stdio for local MCP clients that spawn a child process, and can instead
// serve the MCP Streamable HTTP transport for remote MCP-capable clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/mcpserver"
)

func main() {
	workspace := flag.String("workspace", ".", "workspace directory that bounds every filesystem operation")
	transport := flag.String("transport", "stdio", `MCP transport to serve: "stdio" or "http"`)
	listen := flag.String("listen", "127.0.0.1:8765", "address to bind for --transport=http (loopback by default; do not bind a non-loopback address without also setting --http-token or otherwise restricting network access)")
	httpPath := flag.String("http-path", mcpserver.DefaultHTTPPath, "endpoint path to serve for --transport=http")
	httpToken := flag.String("http-token", "", "if set, require this bearer token on every --transport=http request")
	flag.Parse()

	logger := log.New(os.Stderr, "coreutils-mcp: ", 0)

	server, err := mcpserver.New(*workspace, mcpserver.DefaultLimits(), logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coreutils-mcp: %s\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("serving %d tools", len(server.ToolNames()))

	switch *transport {
	case "stdio":
		if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "coreutils-mcp: %s\n", err)
			os.Exit(1)
		}
	case "http":
		if err := serveHTTP(ctx, logger, server, *listen, *httpPath, *httpToken); err != nil {
			fmt.Fprintf(os.Stderr, "coreutils-mcp: %s\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "coreutils-mcp: unknown --transport %q (want \"stdio\" or \"http\")\n", *transport)
		os.Exit(2)
	}
}

func serveHTTP(ctx context.Context, logger *log.Logger, server *mcpserver.Server, listen, path, token string) error {
	if shouldWarnUnauthenticated(listen, token) {
		logger.Printf("WARNING: --listen=%s has no --http-token set; every reachable client gets unauthenticated filesystem tool access", listen)
	}

	handler := server.HTTPHandler(mcpserver.HTTPOptions{Path: path, BearerToken: token})
	httpServer := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening for MCP Streamable HTTP on http://%s%s", listen, path)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// shouldWarnUnauthenticated reports whether serveHTTP should warn that a
// --transport=http deployment accepts unauthenticated requests: no
// --http-token was configured, and the bind address is not loopback-only
// (so it is reachable from beyond the local machine once published).
func shouldWarnUnauthenticated(listen, token string) bool {
	if token != "" {
		return false
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	return host != "127.0.0.1" && host != "localhost" && host != "::1"
}
