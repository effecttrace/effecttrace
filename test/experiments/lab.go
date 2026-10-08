// Package experiments runs EffectTrace experiments against the kind lab and
// evaluates the resulting effect graphs against ground truth that the
// harness derives from its own actions and an independent watch of the
// cluster. EffectTrace output is never used to compute ground truth.
package experiments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/effecttrace/effecttrace/demo/agent"
	"github.com/effecttrace/effecttrace/demo/oteltrace"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// Lab endpoints, fixed by deploy/kind/kind-config.yaml.tmpl.
const (
	Cluster   = "effecttrace-lab"
	Context   = "kind-effecttrace-lab"
	APIURL    = "http://127.0.0.1:18080"
	MCPURL    = "http://127.0.0.1:18081/mcp"
	OTLPURL   = "http://127.0.0.1:14318"
	FaultURL  = "http://127.0.0.1:18082"
	Namespace = "shop"
)

// Lab holds clients for the kind lab.
type Lab struct {
	Repo       string
	Kubeconfig string
	Client     kubernetes.Interface
	HTTP       *http.Client
	tp         *sdktrace.TracerProvider
	prop       propagation.TextMapPropagator
	Log        func(format string, a ...any)
}

// NewLab connects to the lab and refuses any cluster that is not the local
// kind API server of effecttrace-lab.
func NewLab(ctx context.Context, repo string, log func(string, ...any)) (*Lab, error) {
	kcfg := filepath.Join(repo, ".lab", "kubeconfig")
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kcfg}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: Context})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("lab kubeconfig: %w (run make demo-up)", err)
	}
	if !strings.HasPrefix(cfg.Host, "https://127.0.0.1:") {
		return nil, fmt.Errorf("refusing: %s does not point at a local kind API server (%s)", Context, cfg.Host)
	}
	cfg.UserAgent = "effecttrace-experiments"
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", OTLPURL); err != nil {
		return nil, err
	}
	tp, prop, err := oteltrace.Setup(ctx, "demo-agent")
	if err != nil {
		return nil, err
	}
	return &Lab{Repo: repo, Kubeconfig: kcfg, Client: client, HTTP: &http.Client{Timeout: 30 * time.Second}, tp: tp, prop: prop, Log: log}, nil
}

// Close flushes telemetry.
func (l *Lab) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.tp.Shutdown(ctx)
}

// MCP calls a tool through the scripted agent and flushes its span.
func (l *Lab) MCP(ctx context.Context, tool string, args map[string]any, dropContext bool) (agent.Result, error) {
	a := &agent.Agent{Endpoint: MCPURL, Tracer: l.tp.Tracer("effecttrace-experiments"), Propagator: l.prop, DropContext: dropContext}
	res, err := a.Call(ctx, tool, args)
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.tp.ForceFlush(fctx)
	return res, err
}

var auditIDRE = regexp.MustCompile(`(?m)^\s*Audit-Id:\s*([0-9a-f-]{36})\s*$`)

// Kubectl runs kubectl against the lab only and returns the Audit-IDs the
// API server sent back (parsed from -v=8 output).
func (l *Lab) Kubectl(ctx context.Context, args ...string) ([]string, string, error) {
	full := append([]string{"--kubeconfig", l.Kubeconfig, "--context", Context, "-v=8"}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ids []string
	for _, m := range auditIDRE.FindAllStringSubmatch(stderr.String(), -1) {
		ids = append(ids, m[1])
	}
	if err != nil {
		tail := stderr.String()
		if i := strings.LastIndex(tail, "\n"); len(tail) > 400 && i > 0 {
			tail = tail[len(tail)-400:]
		}
		return ids, stdout.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, tail)
	}
	return ids, stdout.String(), nil
}

// KubectlQuiet runs kubectl without verbose logging.
func (l *Lab) KubectlQuiet(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", l.Kubeconfig, "--context", Context}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// Fault injects a runtime latency/error fault into a shop service without
// any Kubernetes change.
func (l *Lab) Fault(ctx context.Context, service string, latencyMS int, errorRate float64, d time.Duration) error {
	q := url.Values{"service": {service}, "latency_ms": {fmt.Sprint(latencyMS)}, "error_rate": {fmt.Sprint(errorRate)}, "duration": {d.String()}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, FaultURL+"/fault?"+q.Encode(), nil)
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fault relay: %s", b)
	}
	return nil
}

// ErrNotFound is returned when EffectTrace does not know an action.
var ErrNotFound = errors.New("not found")

// Get performs a GET against the EffectTrace API and reports its latency.
func (l *Lab) Get(ctx context.Context, path string) ([]byte, time.Duration, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, APIURL+path, nil)
	start := time.Now()
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	d := time.Since(start)
	if resp.StatusCode == http.StatusNotFound {
		return nil, d, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, d, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, b)
	}
	return b, d, err
}

// ActionForTrace waits until EffectTrace reports exactly one action for a
// trace and returns its ID.
func (l *Lab) ActionForTrace(ctx context.Context, traceID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, _, err := l.Get(ctx, "/api/v1/traces/"+traceID+"/actions")
		if err == nil {
			var r struct {
				Actions []string `json:"actions"`
			}
			if json.Unmarshal(b, &r) == nil && len(r.Actions) == 1 {
				return r.Actions[0], nil
			}
		}
		if err := pause(ctx, time.Second); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no action for trace %s within %s", traceID, timeout)
}

// ActionForAudit waits until one of the given audit IDs is an action.
func (l *Lab) ActionForAudit(ctx context.Context, ids []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			if _, _, err := l.Get(ctx, "/api/v1/effects/k8s-"+id); err == nil {
				return "k8s-" + id, nil
			}
		}
		if err := pause(ctx, time.Second); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no action for audit IDs %v within %s", ids, timeout)
}

// GraphPoll records timing while waiting for a graph to complete.
type GraphPoll struct {
	Graph          *model.EffectGraph
	FirstEffect    time.Duration // action start (harness clock) to first STRUCTURAL edge seen
	Complete       time.Duration // action start to COMPLETE seen
	QueryLatencies []time.Duration
}

// WaitComplete polls a graph until COMPLETE or timeout.
func (l *Lab) WaitComplete(ctx context.Context, actionID string, started time.Time, timeout time.Duration) (GraphPoll, error) {
	var gp GraphPoll
	deadline := time.Now().Add(timeout)
	for {
		b, lat, err := l.Get(ctx, "/api/v1/effects/"+url.PathEscape(actionID))
		if err == nil {
			gp.QueryLatencies = append(gp.QueryLatencies, lat)
			g, err := model.UnmarshalGraph(b)
			if err != nil {
				return gp, fmt.Errorf("graph %s failed validation: %w", actionID, err)
			}
			gp.Graph = g
			if gp.FirstEffect == 0 {
				for _, e := range g.Edges {
					if e.Evidence == model.EvidenceStructural {
						gp.FirstEffect = time.Since(started)
						break
					}
				}
			}
			if g.Status == model.StatusComplete {
				gp.Complete = time.Since(started)
				return gp, nil
			}
		}
		if time.Now().After(deadline) {
			if gp.Graph != nil {
				return gp, fmt.Errorf("graph %s still %s after %s", actionID, gp.Graph.Status, timeout)
			}
			return gp, fmt.Errorf("graph %s not available: %w", actionID, err)
		}
		if err := pause(ctx, time.Second); err != nil {
			return gp, err
		}
	}
}

// pause waits for d or until ctx is done. The harness waits on real-world
// time (controllers, scrapes); it never uses sleeping to make code correct.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
