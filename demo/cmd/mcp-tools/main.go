// Command mcp-tools serves the demo MCP tool server over Streamable HTTP.
// It is the demo ACTOR: it holds mutation rights in the demo namespace only.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/effecttrace/effecttrace/demo/mcptools"
	"github.com/effecttrace/effecttrace/demo/oteltrace"
	"github.com/effecttrace/effecttrace/pkg/k8sinstrument"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("mcp-tools failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tp, prop, err := oteltrace.Setup(ctx, "demo-tools")
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}()
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cfg.UserAgent = "effecttrace-demo-tools/v0.1.0"
	instrumented := os.Getenv("UNINSTRUMENTED_KUBERNETES_CLIENT") != "true"
	if instrumented {
		cfg.Wrap(k8sinstrument.Wrap)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	allowed := strings.Split(os.Getenv("ALLOWED_NAMESPACES"), ",")
	tools := &mcptools.Server{Client: client, Tracer: tp.Tracer("effecttrace-demo-tools"), Propagator: prop, AllowedNamespaces: allowed}
	srv := mcp.NewServer(&mcp.Implementation{Name: "effecttrace-demo-tools", Version: "v0.1.0"}, nil)
	srv.AddReceivingMiddleware(tools.Middleware)
	tools.Register(srv)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true, MaxRequestBodyBytes: 1 << 20}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	hs := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	logger.Info("demo MCP tool server started", "instrumented_kubernetes_client", instrumented, "namespaces", allowed)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
