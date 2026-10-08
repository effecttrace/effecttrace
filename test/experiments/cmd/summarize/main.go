// Command summarize rebuilds test-results/summary.json from results.json.
// Values are derived from the artifact only; nothing is typed by hand.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/effecttrace/effecttrace/test/experiments"
)

func main() {
	dir := flag.String("dir", "test-results", "results directory")
	flag.Parse()
	b, err := os.ReadFile(filepath.Join(*dir, "results.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var rf experiments.ResultsFile
	if err := json.Unmarshal(b, &rf); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	s := experiments.Summarize(&rf)
	if err := experiments.WriteJSON(filepath.Join(*dir, "summary.json"), s); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("%d scenarios: %d pass, %d fail, %d error, %d unsupported\n", s.Scenarios["total"], s.Scenarios["pass"], s.Scenarios["fail"], s.Scenarios["error"], s.Scenarios["unsupported"])
}
