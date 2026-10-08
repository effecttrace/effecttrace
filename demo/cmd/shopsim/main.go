// Command shopsim is a deliberately small synthetic microservice used by the
// EffectTrace lab. One binary plays every role (frontend, checkout, payments,
// inventory, loadgen) so that the reference workload stays reproducible.
//
// Behaviour that matters for experiments:
//   - every request is counted in shop_http_requests_total{service,code} and
//     timed in shop_http_request_duration_seconds{service};
//   - a freshly started process adds WARMUP_LATENCY_MS for WARMUP seconds,
//     which makes a rollout visible in latency (a documented, synthetic
//     effect of this demo application);
//   - /admin/fault injects latency or errors at runtime WITHOUT any
//     Kubernetes change, which is how experiments create unrelated incidents.
//
// All data is fictional.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type fault struct {
	mu        sync.Mutex
	latency   time.Duration
	errorRate float64
	until     time.Time
}

func (f *fault) set(lat time.Duration, rate float64, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency, f.errorRate, f.until = lat, rate, time.Now().Add(d)
}

func (f *fault) get() (time.Duration, float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Now().After(f.until) {
		return 0, 0
	}
	return f.latency, f.errorRate
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(k), 64)
	if err != nil {
		return def
	}
	return v
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	role := env("ROLE", "checkout")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if role == "loadgen" {
		err = runLoadgen(ctx, logger)
	} else {
		err = runService(ctx, role, logger)
	}
	if err != nil {
		logger.Error("shopsim failed", "err", err)
		os.Exit(1)
	}
}

func runService(ctx context.Context, role string, logger *slog.Logger) error {
	reg := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "shop_http_requests_total", Help: "Requests served."}, []string{"service", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "shop_http_request_duration_seconds", Help: "Request latency.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 2},
	}, []string{"service"})
	reg.MustRegister(requests, duration)

	baseLatency := time.Duration(envFloat("BASE_LATENCY_MS", 8)) * time.Millisecond
	errorRate := envFloat("ERROR_RATE", 0)
	warmup := time.Duration(envFloat("WARMUP_SECONDS", 20)) * time.Second
	warmupLatency := time.Duration(envFloat("WARMUP_LATENCY_MS", 60)) * time.Millisecond
	readyDelay := time.Duration(envFloat("READY_DELAY_SECONDS", 2)) * time.Second
	var downstream []string
	for _, d := range strings.Split(os.Getenv("DOWNSTREAMS"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			downstream = append(downstream, d)
		}
	}
	started := time.Now()
	f := &fault{}
	client := &http.Client{Timeout: 2 * time.Second}
	var shuttingDown sync.Once
	stopping := make(chan struct{})

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-stopping:
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		default:
		}
		if time.Since(started) < readyDelay {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("POST /admin/fault", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lat, _ := strconv.Atoi(q.Get("latency_ms"))
		rate, _ := strconv.ParseFloat(q.Get("error_rate"), 64)
		d, err := time.ParseDuration(q.Get("duration"))
		if err != nil || d <= 0 || d > 10*time.Minute || lat < 0 || lat > 5000 || rate < 0 || rate > 1 {
			http.Error(w, "invalid fault", http.StatusBadRequest)
			return
		}
		f.set(time.Duration(lat)*time.Millisecond, rate, d)
		logger.Info("fault injected", "latency_ms", lat, "error_rate", rate, "duration", d)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		code := http.StatusOK
		defer func() {
			requests.WithLabelValues(role, strconv.Itoa(code)).Inc()
			duration.WithLabelValues(role).Observe(time.Since(start).Seconds())
		}()
		lat := baseLatency + time.Duration(rand.Int64N(int64(baseLatency)+1)) // #nosec G404 -- simulated jitter, not security relevant
		if time.Since(started) < warmup {
			lat += warmupLatency
		}
		fl, fr := f.get()
		lat += fl
		time.Sleep(lat)                    // simulated work, not a synchronization mechanism
		if rand.Float64() < errorRate+fr { // #nosec G404 -- simulated failures, not security relevant
			code = http.StatusInternalServerError
			http.Error(w, "simulated failure", code)
			return
		}
		for _, d := range downstream {
			dreq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, d+"/work", nil) // #nosec G704 -- downstream URLs come from the Deployment DOWNSTREAMS setting
			if err != nil {
				code = http.StatusInternalServerError
				http.Error(w, "bad downstream", code)
				return
			}
			resp, err := client.Do(dreq) // #nosec G704 -- downstream URLs come from the Deployment's DOWNSTREAMS setting
			if err != nil {
				code = http.StatusBadGateway
				http.Error(w, "downstream unavailable", code)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 500 {
				code = http.StatusBadGateway
				http.Error(w, "downstream failed", code)
				return
			}
		}
		fmt.Fprintf(w, "%s ok\n", role)
	})

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { // #nosec G118 -- server shutdown goroutine, not request-scoped
		<-ctx.Done()
		shuttingDown.Do(func() { close(stopping) })
		// Give endpoints time to drop this Pod before closing listeners.
		drain := time.NewTimer(3 * time.Second)
		<-drain.C
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // #nosec G118 -- shutdown outlives the cancelled context
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("shopsim service started", "role", role, "downstreams", downstream)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runLoadgen(ctx context.Context, logger *slog.Logger) error {
	target := env("TARGET", "http://frontend:8080")
	rps := envFloat("RPS", 20)
	client := &http.Client{Timeout: 3 * time.Second}
	allowed := map[string]bool{}
	for _, s := range strings.Split(env("FAULT_TARGETS", "frontend,checkout,payments,inventory"), ",") {
		allowed[strings.TrimSpace(s)] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	// Relay fault injection to a fixed allowlist of in-namespace services.
	mux.HandleFunc("POST /fault", func(w http.ResponseWriter, r *http.Request) {
		svc := r.URL.Query().Get("service")
		if !allowed[svc] {
			http.Error(w, "unknown service", http.StatusBadRequest)
			return
		}
		q := url.Values{}
		for _, k := range []string{"latency_ms", "error_rate", "duration"} {
			q.Set(k, r.URL.Query().Get(k))
		}
		// #nosec G704 -- svc is checked against the FAULT_TARGETS allowlist above
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://"+svc+":8080/admin/fault?"+q.Encode(), nil)
		// Every replica must receive the fault; the Service picks one, so
		// repeat enough times to cover small replica counts.
		ok := 0
		for range 12 {
			resp, err := client.Do(req.Clone(r.Context())) // #nosec G704 -- allowlisted in-namespace service
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					ok++
				}
				_ = resp.Body.Close()
			}
		}
		if ok == 0 {
			http.Error(w, "fault relay failed", http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "fault relayed to %s (%d deliveries)\n", svc, ok) // #nosec G705 -- plain-text response; svc is allowlisted
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { // #nosec G118 -- server shutdown goroutine, not request-scoped
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // #nosec G118 -- shutdown outlives the cancelled context
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		interval := time.Duration(float64(time.Second) / rps)
		t := time.NewTicker(interval)
		defer t.Stop()
		sem := make(chan struct{}, 64)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case sem <- struct{}{}:
					go func() {
						defer func() { <-sem }()
						req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/work", nil)
						if err != nil {
							return
						}
						if resp, err := client.Do(req); err == nil { // #nosec G704 -- TARGET comes from the Deployment spec
							_, _ = io.Copy(io.Discard, resp.Body)
							_ = resp.Body.Close()
						}
					}()
				default: // bounded concurrency: skip when saturated
				}
			}
		}
	}()
	logger.Info("loadgen started", "target", target, "rps", rps)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
