// Command coreutils-mcp serves the read-only coreutils tool set over stdio
// using the Model Context Protocol. Protocol messages use stdin and stdout
// exclusively; all diagnostics go to stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/groovy-sky/groovy-agent/internal/mcpserver"
)

func main() {
	workspace := flag.String("workspace", ".", "workspace directory that bounds every filesystem operation")
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
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "coreutils-mcp: %s\n", err)
		os.Exit(1)
	}
}
