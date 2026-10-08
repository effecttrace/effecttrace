// Command agent is the scripted MCP client of the EffectTrace demo. It calls
// exactly the tool it is told to call; no language model is involved.
//
//	agent --endpoint http://127.0.0.1:18081/mcp restart_workload namespace=shop deployment=checkout
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/demo/agent"
	"github.com/effecttrace/effecttrace/demo/oteltrace"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:18081/mcp", "MCP Streamable HTTP endpoint")
	noContext := flag.Bool("no-trace-context", false, "do not propagate trace context in _meta")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agent [--endpoint URL] <tool> key=value ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	args := map[string]any{}
	for _, kv := range flag.Args()[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "argument %q is not key=value\n", kv)
			os.Exit(2)
		}
		if n, err := strconv.Atoi(v); err == nil && k == "replicas" {
			args[k] = n
		} else {
			args[k] = v
		}
	}
	ctx := context.Background()
	tp, prop, err := oteltrace.Setup(ctx, "demo-agent")
	if err != nil {
		fmt.Fprintln(os.Stderr, "telemetry setup:", err)
		os.Exit(1)
	}
	a := &agent.Agent{Endpoint: *endpoint, Tracer: tp.Tracer("effecttrace-demo-agent"), Propagator: prop, DropContext: *noContext}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	res, callErr := a.Call(cctx, flag.Arg(0), args)
	cancel()
	fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
	_ = tp.ForceFlush(fctx)
	_ = tp.Shutdown(fctx)
	fcancel()
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	if callErr != nil {
		fmt.Fprintln(os.Stderr, "error:", callErr)
		os.Exit(1)
	}
}
