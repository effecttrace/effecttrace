package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
)

func src(i int) obs.Record {
	return obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{Source: "s", At: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC), Healthy: true}}
}

func TestOfferAppliesBackpressure(t *testing.T) {
	p := New(store.New(store.DefaultConfig()), 2, nil, nil, slog.New(slog.DiscardHandler))
	if !p.Offer(src(1)) || !p.Offer(src(2)) {
		t.Fatal("queue rejected records below capacity")
	}
	if p.Offer(src(3)) {
		t.Fatal("full queue accepted a record")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Submit(ctx, src(4)); err == nil {
		t.Fatal("Submit on a full queue did not block until the deadline")
	}
}

func TestRunDrainsAndRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.jsonl")
	rec, err := NewRecorder(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(store.DefaultConfig())
	p := New(st, 10, rec, nil, slog.New(slog.DiscardHandler))
	for i := range 5 {
		p.Offer(src(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx) // drains queued records after cancellation
	if p.Offer(src(9)) {
		t.Fatal("closed pipeline accepted a record")
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	recs, bad, err := ReadRecording(f)
	if err != nil || bad != 0 || len(recs) != 5 {
		t.Fatalf("recording: %d records, %d bad, %v", len(recs), bad, err)
	}
	// Reopening appends instead of truncating.
	rec2, _ := NewRecorder(path, 1<<20)
	rec2.Write(src(42))
	rec2.Close()
	b, _ := os.ReadFile(path)
	if n := bytes.Count(b, []byte("\n")); n != 6 {
		t.Fatalf("lines after reopen = %d, want 6", n)
	}
}

func TestRecorderStopsAtLimit(t *testing.T) {
	rec, _ := NewRecorder(filepath.Join(t.TempDir(), "r"), 200)
	var stopped bool
	for i := range 20 {
		if err := rec.Write(src(i)); err != nil {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("recorder never reported reaching its size limit")
	}
	rec.Close()
}
