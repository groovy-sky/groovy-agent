package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/openaiproxy"
)

func main() {
	listenAddr := envOrDefault("OPENAI_PROXY_LISTEN_ADDR", "0.0.0.0:8080")
	upstreamURL := envOrDefault("OPENAI_PROXY_UPSTREAM_URL", "http://127.0.0.1:18080")
	mcpEnabled := envBool("OPENAI_PROXY_MCP_ENABLED", true)
	maxCalls := envInt("OPENAI_PROXY_MAX_TOOL_CALLS_PER_TURN", 3)
	disableDuplicates := envBool("OPENAI_PROXY_DISABLE_DUPLICATE_TOOL_CALLS", true)
	cacheTTL := time.Duration(envInt("OPENAI_PROXY_TOOLS_CACHE_SECONDS", 10)) * time.Second

	logger := log.New(os.Stderr, "openai-mcp-proxy: ", 0)
	proxy, err := openaiproxy.New(openaiproxy.Config{
		UpstreamURL:               upstreamURL,
		MCPEnabled:                mcpEnabled,
		MaxToolCallsPerTurn:       maxCalls,
		ToolsCacheTTL:             cacheTTL,
		DisableDuplicateToolCalls: disableDuplicates,
	}, logger)
	if err != nil {
		logger.Fatal(err)
	}

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s and proxying to %s", listenAddr, upstreamURL)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value != "0" && value != "false" && value != "no"
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
