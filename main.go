package main

import (
	"context"
	"fmt"
	"os"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/agent"
	"github.com/groovy-sky/groovy-agent/internal/mcp"
)

func main() {
	if len(os.Args) == 1 || os.Args[1] == "mcp" {
		if err := mcp.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "agent" {
		if err := agent.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := coreutils.Run(context.Background(), os.Args[1], os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
