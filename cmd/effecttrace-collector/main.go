// Command effecttrace-collector ingests OpenTelemetry spans, Kubernetes audit
// events, Kubernetes watch notifications and Prometheus signals, and serves
// evidence-graded effect graphs over a read-only HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/effecttrace/effecttrace/internal/api"
	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/metrics"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/otelexport"
	"github.com/effecttrace/effecttrace/internal/pipeline"
	"github.com/effecttrace/effecttrace/internal/privacy"
	"github.com/effecttrace/effecttrace/internal/source/audit"
	"github.com/effecttrace/effecttrace/internal/source/kube"
	"github.com/effecttrace/effecttrace/internal/source/otlp"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/telemetry"
	"github.com/effecttrace/effecttrace/internal/version"
)

type config struct {
	apiListen      string
	otlpListen     string
	auditLog       string
	auditFromEnd   bool
	kube           string
	kubeconfig     string
	kubeContext    string
	namespaces     string
	promURL        string
	signalsFile    string
	promStep       time.Duration
	recordPath     string
	recordMax      int64
	exportEndpoint string
	exportInsecure bool
	identityMode   string
	identityKeyEnv string
	tokenFile      string
	queueSize      int
	logLevel       string
	maxReconcile   time.Duration
	settle         time.Duration
	baseline       time.Duration
	retention      time.Duration
}

func main() {
	var c config
	fs := flag.NewFlagSet("effecttrace-collector", flag.ExitOnError)
	fs.StringVar(&c.apiListen, "api-listen", ":8080", "address for the read-only API, /metrics and health endpoints")
	fs.StringVar(&c.otlpListen, "otlp-listen", ":4318", "address for the OTLP/HTTP trace receiver (empty disables)")
	fs.StringVar(&c.auditLog, "audit-log", "", "path of the kube-apiserver audit log (JSON lines); empty disables")
	fs.BoolVar(&c.auditFromEnd, "audit-from-end", false, "start reading the audit log at its end instead of its beginning")
	fs.StringVar(&c.kube, "kube", "in-cluster", "Kubernetes access: in-cluster, kubeconfig, or none")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig path when --kube=kubeconfig")
	fs.StringVar(&c.kubeContext, "kube-context", "", "kubeconfig context; REQUIRED when --kube=kubeconfig")
	fs.StringVar(&c.namespaces, "namespaces", "", "comma-separated namespaces to watch (default: all)")
	fs.StringVar(&c.promURL, "prometheus-url", "", "Prometheus base URL for signal evaluation; empty disables")
	fs.StringVar(&c.signalsFile, "signals", "", "YAML file defining telemetry signals")
	fs.DurationVar(&c.promStep, "prometheus-step", 2*time.Second, "query_range step")
	fs.StringVar(&c.recordPath, "record", "", "write applied observations to this JSON-lines file for replay")
	fs.Int64Var(&c.recordMax, "record-max-bytes", 256<<20, "maximum recording size")
	fs.StringVar(&c.exportEndpoint, "otlp-export-endpoint", "", "host:port to export completed graphs as OTLP/HTTP spans")
	fs.BoolVar(&c.exportInsecure, "otlp-export-insecure", false, "use plain HTTP for graph export")
	fs.StringVar(&c.identityMode, "identity-mode", string(privacy.IdentityPseudonymize), "pseudonymize or keep human usernames")
	fs.StringVar(&c.identityKeyEnv, "identity-key-env", "EFFECTTRACE_IDENTITY_KEY", "environment variable holding the pseudonymization key")
	fs.StringVar(&c.tokenFile, "api-token-file", "", "file containing a bearer token required on /api/*")
	fs.IntVar(&c.queueSize, "queue-size", 10_000, "ingest queue capacity")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.DurationVar(&c.maxReconcile, "max-reconcile", correlate.DefaultConfig().MaxReconcile, "maximum reconciliation window")
	fs.DurationVar(&c.settle, "settle", correlate.DefaultConfig().Settle, "window extension after a stable status")
	fs.DurationVar(&c.baseline, "baseline", correlate.DefaultConfig().BaselineWindow, "telemetry baseline before an action")
	fs.DurationVar(&c.retention, "retention", store.DefaultConfig().Retention, "how long observations are kept")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("effecttrace-collector", version.String())
		return
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid --log-level")
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c, logger); err != nil {
		logger.Error("collector failed", "err", err)
		os.Exit(1)
	}
}

func kubeClient(c config) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	switch c.kube {
	case "none":
		return nil, nil
	case "in-cluster":
		cfg, err = rest.InClusterConfig()
	case "kubeconfig":
		if c.kubeContext == "" {
			return nil, errors.New("--kube=kubeconfig requires an explicit --kube-context; EffectTrace never uses the current context implicitly")
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if c.kubeconfig != "" {
			rules.ExplicitPath = c.kubeconfig
		}
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: c.kubeContext}).ClientConfig()
	default:
		return nil, fmt.Errorf("unknown --kube %q", c.kube)
	}
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "effecttrace-collector/" + version.Version
	return kubernetes.NewForConfig(cfg)
}

func loadSignals(path string) ([]telemetry.Signal, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied --signals path
	if err != nil {
		return nil, err
	}
	var doc struct {
		Signals []telemetry.Signal `json:"signals"`
	}
	if err := yaml.UnmarshalStrict(b, &doc); err != nil {
		return nil, fmt.Errorf("parse signals: %w", err)
	}
	for _, s := range doc.Signals {
		if err := s.Validate(); err != nil {
			return nil, err
		}
	}
	return doc.Signals, nil
}

func run(ctx context.Context, c config, logger *slog.Logger) error {
	stCfg := store.DefaultConfig()
	stCfg.Retention = c.retention
	st := store.New(stCfg)

	var rec *pipeline.Recorder
	if c.recordPath != "" {
		r, err := pipeline.NewRecorder(c.recordPath, c.recordMax)
		if err != nil {
			return err
		}
		rec = r
		defer func() { _ = rec.Close() }()
		go rec.FlushEvery(ctx, time.Second)
	}

	receiver := &otlp.Receiver{Logger: logger}
	var pl *pipeline.Pipeline
	m := metrics.New(
		func() float64 { return float64(pl.Depth()) },
		func() float64 { return float64(pl.Capacity()) },
		func(result string) float64 {
			switch result {
			case "accepted":
				return float64(receiver.Stats.Accepted.Load())
			case "rejected":
				return float64(receiver.Stats.Rejected.Load())
			default:
				return float64(receiver.Stats.Throttled.Load())
			}
		})
	pl = pipeline.New(st, c.queueSize, rec, m, logger)
	receiver.Offer = pl.Offer

	ccfg := correlate.DefaultConfig()
	ccfg.MaxReconcile, ccfg.Settle, ccfg.BaselineWindow = c.maxReconcile, c.settle, c.baseline

	var eval *telemetry.Evaluator
	if c.promURL != "" {
		if c.signalsFile == "" {
			return errors.New("--prometheus-url requires --signals")
		}
		sigs, err := loadSignals(c.signalsFile)
		if err != nil {
			return err
		}
		eval = &telemetry.Evaluator{BaseURL: c.promURL, Signals: sigs, Step: c.promStep, Client: &http.Client{Timeout: 10 * time.Second}}
	}
	engine := correlate.NewEngine(st, ccfg, correlate.Options{TelemetryConfigured: eval != nil})

	token := ""
	if c.tokenFile != "" {
		b, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	policy := privacy.Policy{Mode: privacy.IdentityMode(c.identityMode), Key: []byte(os.Getenv(c.identityKeyEnv))}
	if policy.Mode != privacy.IdentityKeep && policy.Mode != privacy.IdentityPseudonymize {
		return fmt.Errorf("invalid --identity-mode %q", c.identityMode)
	}
	if policy.Mode == privacy.IdentityPseudonymize && len(policy.Key) == 0 {
		// Without a key, pseudonyms would be plain hashes that anyone could
		// reverse by hashing candidate usernames. Use a random per-process
		// key instead; pseudonyms are then stable only until restart.
		key, err := privacy.RandomKey()
		if err != nil {
			return err
		}
		policy.Key = key
		logger.Warn("no pseudonymization key configured; using a random key, so pseudonyms change when the collector restarts", "env", c.identityKeyEnv)
	} else if policy.Mode == privacy.IdentityPseudonymize && len(policy.Key) < 16 {
		return fmt.Errorf("%s must hold at least 16 bytes", c.identityKeyEnv)
	}

	var exporter *otelexport.Exporter
	if c.exportEndpoint != "" {
		e, err := otelexport.New(ctx, c.exportEndpoint, c.exportInsecure, version.Version)
		if err != nil {
			return err
		}
		exporter = e
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exporter.Shutdown(sctx)
		}()
	}

	client, err := kubeClient(c)
	if err != nil {
		return err
	}

	var ready atomic.Bool
	pctx, pcancel := context.WithCancel(context.Background())
	var pwg sync.WaitGroup
	pwg.Add(1)
	go func() { defer pwg.Done(); pl.Run(pctx) }()
	defer func() { pcancel(); pwg.Wait() }()

	var wg sync.WaitGroup
	errc := make(chan error, 8)
	start := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	if c.auditLog != "" {
		t := &audit.Tailer{Path: c.auditLog, Policy: policy, FromEnd: c.auditFromEnd, Logger: logger, Sink: pl.Submit}
		start("audit", func() error { return t.Run(ctx) })
	}
	if c.otlpListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/v1/traces", receiver)
		_ = pl.Submit(ctx, obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{Source: "otlp", At: time.Now().UTC(), Healthy: true, StartedAt: time.Now().UTC(), Detail: "receiver listening"}})
		start("otlp", func() error { return otlp.ListenAndServe(ctx, c.otlpListen, mux) })
	}
	synced := make(chan struct{})
	if client != nil {
		var nss []string
		for _, ns := range strings.Split(c.namespaces, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				nss = append(nss, ns)
			}
		}
		w := &kube.Watcher{Client: client, Namespaces: nss, Logger: logger, Sink: pl.Submit}
		start("kube", func() error { return w.Run(ctx, synced) })
		go func() {
			select {
			case <-synced:
				ready.Store(true)
				logger.Info("kubernetes caches synced", "scope", w.String())
			case <-ctx.Done():
			}
		}()
	} else {
		ready.Store(true)
	}

	start("loops", func() error { return loops(ctx, engine, st, pl, eval, exporter, m, logger) })

	srv := &api.Server{
		Engine: engine, Store: st, Version: version.String(), Token: token, Ready: ready.Load,
		ObserveBuild: func(d time.Duration) { m.GraphBuild.Observe(d.Seconds()) },
	}
	handler := srv.Handler(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	httpSrv := &http.Server{Addr: c.apiListen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	start("api", func() error {
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(sctx)
		}()
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	logger.Info("effecttrace collector started", "version", version.String(), "api", c.apiListen, "otlp", c.otlpListen,
		"audit", c.auditLog != "", "kube", c.kube, "prometheus", eval != nil, "record", c.recordPath != "")

	select {
	case <-ctx.Done():
	case err := <-errc:
		logger.Error("component failed", "err", err)
		wg.Wait()
		return err
	}
	wg.Wait()
	if rec != nil {
		_ = rec.Flush()
	}
	logger.Info("effecttrace collector stopped")
	return nil
}

// loops runs periodic work: telemetry evaluation, graph summaries and export,
// and retention pruning.
func loops(ctx context.Context, engine *correlate.Engine, st *store.Store, pl *pipeline.Pipeline, eval *telemetry.Evaluator,
	exporter *otelexport.Exporter, m *metrics.Metrics, logger *slog.Logger) error {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	prune := time.NewTicker(time.Minute)
	defer prune.Stop()
	exported := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-prune.C:
			st.Prune(time.Now())
		case <-tick.C:
			now := time.Now()
			if eval != nil {
				for _, t := range engine.PendingTelemetry(now) {
					results := eval.Evaluate(ctx, t)
					healthy := true
					for _, r := range results {
						res := "unchanged"
						switch {
						case r.Error != "":
							res, healthy = "error", false
						case r.Samples == 0:
							res = "nodata"
						case r.Changed:
							res = "changed"
						}
						m.Telemetry.WithLabelValues(res).Inc()
						if err := pl.Submit(ctx, obs.Record{Kind: obs.KindMetric, Metric: &r}); err != nil {
							return nil
						}
					}
					detail := "signals evaluated"
					if !healthy {
						detail = "some signal queries failed"
					}
					_ = pl.Submit(ctx, obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{Source: "prometheus", At: time.Now().UTC(), Healthy: healthy, Detail: detail}})
				}
			}
			summarize(ctx, engine, now, exporter, exported, m, logger)
		}
	}
}

func summarize(ctx context.Context, engine *correlate.Engine, now time.Time, exporter *otelexport.Exporter, exported map[string]bool, m *metrics.Metrics, logger *slog.Logger) {
	actions := engine.Actions(now, time.Time{})
	m.Graphs.Reset()
	m.Actions.Reset()
	m.Edges.Reset()
	amb := 0
	present := map[string]bool{}
	for _, a := range actions {
		present[a.ID] = true
		m.Graphs.WithLabelValues(string(a.Status)).Inc()
		m.Actions.WithLabelValues(string(a.Kind)).Inc()
		for et, n := range a.Edges {
			m.Edges.WithLabelValues(string(et)).Add(float64(n))
		}
		amb += a.Ambiguities
		if exporter != nil && a.Status == "COMPLETE" && !exported[a.ID] {
			g, err := engine.Graph(now, a.ID)
			if err != nil {
				m.ExportedGraph.WithLabelValues("error").Inc()
				continue
			}
			exporter.Export(ctx, g)
			exported[a.ID] = true
			m.ExportedGraph.WithLabelValues("ok").Inc()
		}
	}
	m.Ambiguities.Set(float64(amb))
	for id := range exported {
		if !present[id] {
			delete(exported, id)
		}
	}
	logger.Debug("summary", "actions", len(actions))
}
