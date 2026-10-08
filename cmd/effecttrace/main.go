// Command effecttrace queries an EffectTrace collector, explains effect
// graphs and replays recordings offline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/pipeline"
	"github.com/effecttrace/effecttrace/internal/render"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/version"
	"github.com/effecttrace/effecttrace/pkg/model"
)

const usage = `effecttrace — trace the effect, not just the call.

Usage:
  effecttrace <command> [flags]

Commands:
  actions   list observed actions
  explain   explain the effect graph of an action
  graph     print an effect graph as json, dot or mermaid
  export    write the canonical JSON effect graph of an action
  observe   follow new actions and explain them as their graphs complete
  replay    rebuild effect graphs offline from a collector recording
  doctor    check collector health and source coverage
  version   print the version

Common flags:
  --server URL   collector API (default $EFFECTTRACE_SERVER or http://127.0.0.1:8080)
  The bearer token, if required, is read from $EFFECTTRACE_TOKEN.

Temporal correlation does not prove causation.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "actions":
		err = cmdActions(ctx, os.Args[2:])
	case "explain":
		err = cmdGraph(ctx, os.Args[2:], "text")
	case "graph":
		err = cmdGraph(ctx, os.Args[2:], "")
	case "export":
		err = cmdGraph(ctx, os.Args[2:], "json")
	case "observe":
		err = cmdObserve(ctx, os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("effecttrace", version.String())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(server string) (*client, error) {
	if server == "" {
		server = os.Getenv("EFFECTTRACE_SERVER")
	}
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid --server %q", server)
	}
	return &client{base: strings.TrimRight(server, "/"), token: os.Getenv("EFFECTTRACE_TOKEN"), http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, model.MaxGraphBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, e.Error)
	}
	return b, nil
}

func colorFlag(fs *flag.FlagSet) *string {
	return fs.String("color", "auto", "auto, always or never")
}

func useColor(mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdActions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("actions", flag.ExitOnError)
	server := fs.String("server", "", "collector API URL")
	since := fs.Duration("since", time.Hour, "show actions newer than this")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	c, err := newClient(*server)
	if err != nil {
		return err
	}
	q := url.Values{"since": {time.Now().Add(-*since).UTC().Format(time.RFC3339Nano)}}
	b, err := c.get(ctx, "/api/v1/actions?"+q.Encode())
	if err != nil {
		return err
	}
	if *asJSON {
		_, err = os.Stdout.Write(b)
		return err
	}
	var resp struct {
		Actions []correlate.ActionSummary `json:"actions"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	if len(resp.Actions) == 0 {
		fmt.Println("no actions observed in the selected period")
		return nil
	}
	fmt.Printf("%-12s %-20s %-10s %-44s %s\n", "STARTED", "KIND", "STATUS", "ACTION", "EDGES (D/T/S/E/C)")
	for _, a := range resp.Actions {
		e := a.Edges
		fmt.Printf("%-12s %-20s %-10s %-44s %d/%d/%d/%d/%d\n  id %s\n", a.StartedAt.Local().Format("15:04:05.000"), a.Kind, a.Status, trunc(a.Name, 44),
			e[model.EvidenceDirect], e[model.EvidenceTraceLink], e[model.EvidenceStructural], e[model.EvidenceEventReference], e[model.EvidenceTemporalCorrelation], a.ID)
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func loadGraph(ctx context.Context, server, action, traceID, file string) (*model.EffectGraph, error) {
	if file != "" {
		f, err := os.Open(file) // #nosec G304 -- operator-supplied --file path
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, model.MaxGraphBytes+1))
		if err != nil {
			return nil, err
		}
		return model.UnmarshalGraph(b)
	}
	c, err := newClient(server)
	if err != nil {
		return nil, err
	}
	if action == "" && traceID != "" {
		b, err := c.get(ctx, "/api/v1/traces/"+url.PathEscape(strings.ToLower(traceID))+"/actions")
		if err != nil {
			return nil, err
		}
		var resp struct {
			Actions []string `json:"actions"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return nil, err
		}
		switch len(resp.Actions) {
		case 0:
			return nil, fmt.Errorf("no action recorded for trace %s", traceID)
		case 1:
			action = resp.Actions[0]
		default:
			return nil, fmt.Errorf("trace %s contains %d actions; choose one with --action: %s", traceID, len(resp.Actions), strings.Join(resp.Actions, ", "))
		}
	}
	if action == "" {
		return nil, errors.New("one of --action, --trace-id or --file is required")
	}
	b, err := c.get(ctx, "/api/v1/effects/"+url.PathEscape(action))
	if err != nil {
		return nil, err
	}
	return model.UnmarshalGraph(b)
}

func cmdGraph(ctx context.Context, args []string, fixed string) error {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	server := fs.String("server", "", "collector API URL")
	action := fs.String("action", "", "action ID")
	traceID := fs.String("trace-id", "", "trace ID of an MCP tool call")
	file := fs.String("file", "", "read a canonical graph JSON file instead of querying a collector")
	format := fs.String("format", "json", "json, dot, mermaid or text")
	out := fs.String("out", "", "write to this file instead of stdout")
	verbose := fs.Bool("verbose", false, "show every node and the reason for every edge")
	color := colorFlag(fs)
	_ = fs.Parse(args)
	if fixed != "" {
		*format = fixed
	}
	g, err := loadGraph(ctx, *server, *action, *traceID, *file)
	if err != nil {
		return err
	}
	var w io.Writer = os.Stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
		*color = "never"
	}
	switch *format {
	case "json":
		b, err := g.MarshalCanonical()
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "dot":
		render.DOT(w, g)
	case "mermaid":
		render.Mermaid(w, g)
	case "text":
		render.Explain(w, g, render.Options{Color: useColor(*color), Verbose: *verbose})
	default:
		return fmt.Errorf("unknown --format %q", *format)
	}
	return nil
}

func cmdObserve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	server := fs.String("server", "", "collector API URL")
	interval := fs.Duration("interval", 2*time.Second, "poll interval")
	color := colorFlag(fs)
	_ = fs.Parse(args)
	c, err := newClient(*server)
	if err != nil {
		return err
	}
	since := time.Now().UTC()
	announced := map[string]bool{}
	explained := map[string]bool{}
	fmt.Fprintf(os.Stderr, "observing new actions from %s (Ctrl-C to stop)\n", c.base)
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		q := url.Values{"since": {since.Format(time.RFC3339Nano)}}
		b, err := c.get(ctx, "/api/v1/actions?"+q.Encode())
		if err == nil {
			var resp struct {
				Actions []correlate.ActionSummary `json:"actions"`
			}
			if json.Unmarshal(b, &resp) == nil {
				for _, a := range resp.Actions {
					if !announced[a.ID] {
						announced[a.ID] = true
						fmt.Printf("observed %s %q (%s)\n", a.Kind, a.Name, a.Status)
					}
					if a.Status == model.StatusComplete && !explained[a.ID] {
						explained[a.ID] = true
						if g, err := loadGraph(ctx, *server, a.ID, "", ""); err == nil {
							fmt.Println(strings.Repeat("─", 72))
							render.Explain(os.Stdout, g, render.Options{Color: useColor(*color)})
							fmt.Println(strings.Repeat("─", 72))
						}
					}
				}
			}
		} else if ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "poll failed:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	recording := fs.String("recording", "", "JSON-lines recording written by effecttrace-collector --record")
	outDir := fs.String("out", "", "write one canonical graph JSON per action into this directory")
	action := fs.String("action", "", "explain only this action")
	settle := fs.Duration("close-after", time.Hour, "evaluate windows as of this long after the last record")
	color := colorFlag(fs)
	verbose := fs.Bool("verbose", false, "verbose explanation")
	_ = fs.Parse(args)
	if *recording == "" {
		return errors.New("--recording is required")
	}
	f, err := os.Open(*recording) // #nosec G304 -- operator-supplied --recording path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	recs, bad, err := pipeline.ReadRecording(f)
	if err != nil {
		return err
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d invalid record(s)\n", bad)
	}
	engine, now := ReplayEngine(recs)
	now = now.Add(*settle)
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o750); err != nil {
			return err
		}
	}
	for _, a := range engine.Actions(now, time.Time{}) {
		if *action != "" && a.ID != *action {
			continue
		}
		g, err := engine.Graph(now, a.ID)
		if err != nil {
			return err
		}
		if *outDir != "" {
			b, err := g.MarshalCanonical()
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(*outDir, safeName(a.ID)+".json"), b, 0o600); err != nil {
				return err
			}
			continue
		}
		render.Explain(os.Stdout, g, render.Options{Color: useColor(*color), Verbose: *verbose})
		fmt.Println()
	}
	return nil
}

// ReplayEngine loads records into a fresh store and returns an engine and
// the latest record time.
func ReplayEngine(recs []obs.Record) (*correlate.Engine, time.Time) {
	st := store.New(store.DefaultConfig())
	telemetry := false
	for _, r := range recs {
		_, _ = st.Apply(r)
		if r.Kind == obs.KindMetric {
			telemetry = true
		}
	}
	var newest time.Time
	st.Read(func(v store.View) { newest = v.Newest() })
	return correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{TelemetryConfigured: telemetry}), newest
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	server := fs.String("server", "", "collector API URL")
	_ = fs.Parse(args)
	c, err := newClient(*server)
	if err != nil {
		return err
	}
	ok := true
	check := func(name, path string) {
		if _, err := c.get(ctx, path); err != nil {
			fmt.Printf("  %-10s FAIL  %v\n", name, err)
			ok = false
			return
		}
		fmt.Printf("  %-10s ok\n", name)
	}
	fmt.Println("collector", c.base)
	check("healthz", "/healthz")
	check("readyz", "/readyz")
	b, err := c.get(ctx, "/api/v1/status")
	if err != nil {
		fmt.Printf("  %-10s FAIL  %v\n", "status", err)
		return errors.New("collector unreachable or unauthorized")
	}
	var st struct {
		Version string `json:"version"`
		Sources []struct {
			Source  string `json:"source"`
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		} `json:"sources"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	fmt.Println("version", st.Version)
	fmt.Println("sources:")
	for _, s := range st.Sources {
		mark := "ok  "
		if !s.Healthy {
			mark = "MISS"
		}
		fmt.Printf("  %-18s %s %s\n", s.Source, mark, s.Detail)
	}
	fmt.Printf("retained: %d requests, %d spans, %d objects, %d events, %d metric results\n",
		st.Counts["requests"], st.Counts["spans"], st.Counts["objects"], st.Counts["events"], st.Counts["metrics"])
	fmt.Println("note: a missing source reduces coverage; EffectTrace reports what it could not see instead of guessing.")
	if !ok {
		return errors.New("health checks failed")
	}
	return nil
}
