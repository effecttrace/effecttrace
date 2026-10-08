// Command benchmarks runs EffectTrace's synthetic in-process benchmarks and
// writes test-results/benchmarks.json.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/bench"
)

type entry struct {
	Name          string  `json:"name"`
	Actions       int     `json:"concurrentActions"`
	Records       int     `json:"records"`
	Iterations    int     `json:"iterations"`
	NsPerOp       int64   `json:"nsPerOp"`
	MsPerOp       float64 `json:"msPerOp"`
	BytesPerOp    int64   `json:"bytesPerOp"`
	AllocsPerOp   int64   `json:"allocsPerOp"`
	RecordsPerSec float64 `json:"recordsPerSecond,omitempty"`
}

type file struct {
	SchemaVersion string            `json:"schemaVersion"`
	Kind          string            `json:"kind"`
	Description   string            `json:"description"`
	GeneratedAt   time.Time         `json:"generatedAt"`
	Commit        string            `json:"commit"`
	Host          map[string]string `json:"host"`
	Benchmarks    []entry           `json:"benchmarks"`
	MemoryPerAct  map[string]int64  `json:"retainedHeapBytesPerAction"`
}

func main() {
	out := flag.String("out", "test-results/benchmarks.json", "output file")
	flag.Parse()
	testing.Init()
	commit := "uncommitted"
	if b, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(b))
	}
	f := file{
		SchemaVersion: "effecttrace.io/benchmarks/v1",
		Kind:          "synthetic-in-process",
		Description:   "Correlation engine only, fed with generated observation streams (one Deployment with 3 Pods per concurrent agent restart). No cluster, network or collector I/O is involved. Not an end-to-end or production-scale measurement.",
		GeneratedAt:   time.Now().UTC(),
		Commit:        commit,
		Host:          map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": fmt.Sprint(runtime.NumCPU()), "go": runtime.Version()},
		MemoryPerAct:  map[string]int64{},
	}
	for _, n := range []int{1, 10, 50, 100} {
		recs := bench.Scenario(n)
		run := func(name string, fn func(*testing.B)) {
			r := testing.Benchmark(fn)
			e := entry{Name: name, Actions: n, Records: len(recs), Iterations: r.N, NsPerOp: r.NsPerOp(),
				MsPerOp: float64(r.NsPerOp()) / 1e6, BytesPerOp: r.AllocedBytesPerOp(), AllocsPerOp: r.AllocsPerOp()}
			if v, ok := r.Extra["records/s"]; ok {
				e.RecordsPerSec = v
			}
			f.Benchmarks = append(f.Benchmarks, e)
			fmt.Fprintf(os.Stderr, "%-16s actions=%-3d %10.3f ms/op\n", name, n, e.MsPerOp)
		}
		run("ingest", func(b *testing.B) { bench.Ingest(b, recs) })
		run("index_and_graph", func(b *testing.B) { bench.IndexAndGraph(b, recs) })
		run("graph_query", func(b *testing.B) { bench.Query(b, recs) })
		f.MemoryPerAct[fmt.Sprint(n)] = int64(bench.MemoryPerAction(recs, n))
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		panic(err)
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}
