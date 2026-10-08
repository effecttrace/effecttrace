// Command run executes the EffectTrace experiment suite against the kind lab
// and writes test-results/.
//
//	go run ./test/experiments/cmd/run [--only L01,L02] [--list]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/test/experiments"
)

func main() {
	repo := flag.String("repo", ".", "repository root")
	out := flag.String("out", "test-results", "output directory (relative to repo)")
	only := flag.String("only", "", "comma-separated scenario IDs to run")
	list := flag.Bool("list", false, "list scenarios and exit")
	flag.Parse()
	root, err := filepath.Abs(*repo)
	if err != nil {
		fail(err)
	}
	live, replay, unsupported := experiments.LiveScenarios(), experiments.ReplayScenarios(), experiments.UnsupportedScenarios()
	all := append(append(append([]experiments.Scenario{}, live...), replay...), unsupported...)
	if *list {
		for _, s := range all {
			fmt.Printf("%s  %-11s %s\n", s.ID, s.Category, s.Title)
		}
		return
	}
	selected := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
	}
	lab, err := experiments.NewLab(ctx, root, logf)
	if err != nil {
		fail(err)
	}
	defer lab.Close()
	rec, err := experiments.StartRecorder(ctx, lab.Client)
	if err != nil {
		fail(err)
	}
	env := &experiments.Env{Lab: lab, Rec: rec, Log: logf, Prior: map[string]*experiments.Result{}}
	outDir := filepath.Join(root, *out)
	commit, dirty := experiments.Git(root)
	rf := &experiments.ResultsFile{SchemaVersion: "effecttrace.io/results/v1", GeneratedAt: time.Now().UTC(), Commit: commit, Dirty: dirty}
	save := func() {
		if err := experiments.WriteJSON(filepath.Join(outDir, "results.json"), rf); err != nil {
			logf("write results: %v", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(outDir, "graphs")); err != nil {
		fail(err)
	}
	logf("restoring the shop namespace to its declared state")
	if err := env.Restore(ctx); err != nil {
		fail(err)
	}
	checkClock := func() {
		skew, err := experiments.ClockSkew(ctx)
		if err != nil {
			fail(err)
		}
		if skew.Abs() > 1500*time.Millisecond {
			fail(fmt.Errorf("lab clock differs from the host by %s; resynchronize the container VM clock before running experiments", skew.Round(time.Millisecond)))
		}
	}
	for _, s := range all {
		if len(selected) > 0 && !selected[s.ID] {
			continue
		}
		if s.Category == "live" {
			checkClock()
		}
		r := &experiments.Result{ID: s.ID, Title: s.Title, Category: s.Category, Description: s.Description, Started: time.Now().UTC()}
		if s.Category == "unsupported" {
			r.Status = "unsupported"
			rf.Scenarios = append(rf.Scenarios, r)
			save()
			continue
		}
		if s.Quiet {
			logf("%s: quiet period so the telemetry baseline is undisturbed", s.ID)
			if err := quiet(ctx, 70*time.Second); err != nil {
				fail(err)
			}
		}
		logf("%s: %s", s.ID, s.Title)
		sctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		start := time.Now()
		err := s.Run(sctx, env, r)
		cancel()
		r.DurationS = time.Since(start).Seconds()
		switch {
		case err != nil:
			r.Status, r.Error = "error", err.Error()
		case slices.ContainsFunc(r.Checks, func(c experiments.Check) bool { return !c.Pass }):
			r.Status = "fail"
		case len(r.Checks) == 0:
			r.Status, r.Error = "error", "no checks evaluated"
		default:
			r.Status = "pass"
		}
		logf("%s: %s (%.0fs)", s.ID, strings.ToUpper(r.Status), r.DurationS)
		for _, c := range r.Checks {
			if !c.Pass {
				logf("    FAILED %s: %s", c.Name, c.Detail)
			}
		}
		if r.Error != "" {
			logf("    ERROR %s", r.Error)
		}
		for i, o := range r.Actions {
			if o.Graph == nil {
				continue
			}
			b, err := o.Graph.MarshalCanonical()
			if err == nil {
				_ = os.MkdirAll(filepath.Join(outDir, "graphs"), 0o755)
				_ = os.WriteFile(filepath.Join(outDir, "graphs", fmt.Sprintf("%s-%d.json", s.ID, i+1)), b, 0o644)
			}
		}
		env.Prior[s.ID] = r
		rf.Scenarios = append(rf.Scenarios, r)
		save()
		if s.Category == "live" {
			if err := env.Restore(ctx); err != nil {
				logf("restore failed: %v", err)
				if errors.Is(err, context.Canceled) {
					break
				}
			}
		}
	}
	sum := experiments.Summarize(rf)
	if err := experiments.WriteJSON(filepath.Join(outDir, "summary.json"), sum); err != nil {
		fail(err)
	}
	ver, _ := lab.Client.Discovery().ServerVersion()
	kv := "unknown"
	if ver != nil {
		kv = ver.GitVersion
	}
	engine := "podman"
	if _, err := exec.LookPath("docker"); err == nil {
		engine = "docker"
	}
	if err := experiments.WriteJSON(filepath.Join(outDir, "environment.json"), experiments.CaptureEnvironment(root, kv, engineVersion(engine))); err != nil {
		fail(err)
	}
	logf("done: %d pass, %d fail, %d error, %d unsupported", sum.Scenarios["pass"], sum.Scenarios["fail"], sum.Scenarios["error"], sum.Scenarios["unsupported"])
}

func engineVersion(engine string) string {
	out, err := exec.Command(engine, "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return engine
	}
	return engine + " " + strings.TrimSpace(string(out))
}

func quiet(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
