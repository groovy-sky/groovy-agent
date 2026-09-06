package main

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/mcpserver"
)

func TestShouldWarnUnauthenticated(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		token  string
		want   bool
	}{
		{"loopback ipv4 without token warns", "127.0.0.1:8765", "", false},
		{"localhost without token warns", "localhost:8765", "", false},
		{"loopback ipv6 without token warns", "[::1]:8765", "", false},
		{"non-loopback without token warns", "0.0.0.0:8765", "", true},
		{"non-loopback with token does not warn", "0.0.0.0:8765", "secret", false},
		{"loopback with token does not warn", "127.0.0.1:8765", "secret", false},
		{"unparsable address warns", "not-an-address", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldWarnUnauthenticated(tc.listen, tc.token); got != tc.want {
				t.Fatalf("shouldWarnUnauthenticated(%q, %q) = %v, want %v", tc.listen, tc.token, got, tc.want)
			}
		})
	}
}

func newTestMCPServer(t *testing.T) *mcpserver.Server {
	t.Helper()
	server, err := mcpserver.New(t.TempDir(), mcpserver.DefaultLimits(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	return server
}

func TestServeHTTPShutsDownOnContextCancel(t *testing.T) {
	server := newTestMCPServer(t)
	logger := log.New(io.Discard, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveHTTP(ctx, logger, server, "127.0.0.1:0", mcpserver.DefaultHTTPPath, "")
	}()

	// Give the listener goroutine a moment to start before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveHTTP returned an error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not return after context cancellation")
	}
}

func TestServeHTTPReturnsListenError(t *testing.T) {
	server := newTestMCPServer(t)
	logger := log.New(io.Discard, "", 0)

	// Occupy a port, then try to bind the same address again so
	// ListenAndServe fails immediately with a plain listen error.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := serveHTTP(ctx, logger, server, listener.Addr().String(), mcpserver.DefaultHTTPPath, ""); err == nil {
		t.Fatal("expected serveHTTP to return a listen error for an already-bound address")
	}
}
