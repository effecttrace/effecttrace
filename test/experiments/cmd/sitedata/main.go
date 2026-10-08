// Command sitedata exports generated experiment artifacts for the project
// website. The site renders only these files; no number is typed by hand.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// replays are the recorded experiments offered in the website replay, with
// the graph files they use.
var replays = []struct {
	ID, Title, Scenario string
	Graphs              []string
}{
	{"restart", "Restart workload", "L01", []string{"L01-1"}},
	{"scale", "Scale deployment", "L02", []string{"L02-1"}},
	{"bad-image", "Bad image rollout", "L06", []string{"L06-1"}},
	{"unrelated", "Unrelated concurrent change", "L15", []string{"L15-1", "L15-2"}},
	{"concurrent", "Two concurrent actions", "L14", []string{"L14-1", "L14-2"}},
	{"same-workload", "Two actions, same workload", "L16", []string{"L16-1", "L16-2"}},
	{"no-trace-context", "Trace context lost", "L24", []string{"L24-1", "L24-2"}},
}

func main() {
	results := flag.String("results", "test-results", "results directory")
	out := flag.String("out", "../effecttrace.github.io/data", "site data directory")
	flag.Parse()
	if err := os.MkdirAll(filepath.Join(*out, "graphs"), 0o750); err != nil {
		fail(err)
	}
	for _, f := range []string{"summary.json", "results.json", "environment.json", "benchmarks.json"} {
		b, err := os.ReadFile(filepath.Join(*results, f))
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", f, err)
			continue
		}
		if !json.Valid(b) {
			fail(fmt.Errorf("%s is not valid JSON", f))
		}
		if err := os.WriteFile(filepath.Join(*out, f), b, 0o644); err != nil { // #nosec G306 -- public website data
			fail(err)
		}
	}
	type entry struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Scenario string   `json:"scenario"`
		Graphs   []string `json:"graphs"`
	}
	var index []entry
	for _, r := range replays {
		e := entry{ID: r.ID, Title: r.Title, Scenario: r.Scenario}
		for _, g := range r.Graphs {
			b, err := os.ReadFile(filepath.Join(*results, "graphs", g+".json"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "skip graph %s: %v\n", g, err)
				continue
			}
			if err := os.WriteFile(filepath.Join(*out, "graphs", g+".json"), b, 0o644); err != nil { // #nosec G306 -- public website data
				fail(err)
			}
			e.Graphs = append(e.Graphs, g)
		}
		if len(e.Graphs) > 0 {
			index = append(index, e)
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"replays": index}, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "replays.json"), append(b, '\n'), 0o644); err != nil { // #nosec G306 -- public website data
		fail(err)
	}
	fmt.Printf("exported %d replays to %s\n", len(index), *out)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
