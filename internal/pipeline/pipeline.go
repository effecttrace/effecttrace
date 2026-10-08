// Package pipeline moves observations from sources into the store through a
// single bounded queue. Sources that can wait (the audit tailer, informers)
// block when the queue is full; the OTLP receiver uses Offer and returns
// HTTP 503 instead, so producers see explicit backpressure.
package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
)

// ErrClosed is returned when submitting after shutdown.
var ErrClosed = errors.New("pipeline closed")

// Observer receives ingest outcomes, for metrics.
type Observer interface {
	Ingested(kind obs.Kind, result string)
}

// Pipeline is the single writer to the store.
type Pipeline struct {
	st       *store.Store
	queue    chan obs.Record
	rec      *Recorder
	observer Observer
	logger   *slog.Logger
	done     chan struct{}
}

// New returns a pipeline with a queue of the given capacity.
func New(st *store.Store, capacity int, rec *Recorder, observer Observer, logger *slog.Logger) *Pipeline {
	return &Pipeline{st: st, queue: make(chan obs.Record, capacity), rec: rec, observer: observer, logger: logger, done: make(chan struct{})}
}

// Submit enqueues r, blocking while the queue is full.
func (p *Pipeline) Submit(ctx context.Context, r obs.Record) error {
	select {
	case <-p.done:
		return ErrClosed
	default:
	}
	select {
	case p.queue <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	}
}

// Offer enqueues r without blocking and reports whether it was accepted.
func (p *Pipeline) Offer(r obs.Record) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.queue <- r:
		return true
	default:
		if p.observer != nil {
			p.observer.Ingested(r.Kind, "throttled")
		}
		return false
	}
}

// Depth returns the number of queued records.
func (p *Pipeline) Depth() int { return len(p.queue) }

// Capacity returns the queue capacity.
func (p *Pipeline) Capacity() int { return cap(p.queue) }

// Run applies queued records until ctx is cancelled, then drains what is
// already queued.
func (p *Pipeline) Run(ctx context.Context) {
	defer close(p.done)
	for {
		select {
		case r := <-p.queue:
			p.apply(r)
		case <-ctx.Done():
			for {
				select {
				case r := <-p.queue:
					p.apply(r)
				default:
					return
				}
			}
		}
	}
}

func (p *Pipeline) apply(r obs.Record) {
	applied, err := p.st.Apply(r)
	result := "applied"
	switch {
	case err != nil:
		result = "rejected"
		p.logger.Debug("rejected record", "kind", r.Kind, "err", err)
	case !applied:
		result = "duplicate"
	}
	if p.observer != nil {
		p.observer.Ingested(r.Kind, result)
	}
	if err == nil && applied && p.rec != nil {
		if werr := p.rec.Write(r); werr != nil {
			p.logger.Warn("recording stopped", "err", werr)
		}
	}
}

// Recorder writes applied records as JSON lines for offline replay.
type Recorder struct {
	mu       sync.Mutex
	w        *bufio.Writer
	f        *os.File
	written  int64
	maxBytes int64
	stopped  bool
}

// NewRecorder appends to path (created with mode 0600) and records until the
// file reaches maxBytes. Appending keeps earlier observations across
// collector restarts; replay deduplicates repeated records.
func NewRecorder(path string, maxBytes int64) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- operator-supplied --record path
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Recorder{f: f, w: bufio.NewWriter(f), maxBytes: maxBytes, written: st.Size()}, nil
}

// Write appends one record.
func (r *Recorder) Write(rec obs.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if r.written+int64(len(b))+1 > r.maxBytes {
		r.stopped = true
		return fmt.Errorf("recording reached %d bytes", r.maxBytes)
	}
	r.written += int64(len(b)) + 1
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Flush writes buffered data.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.w.Flush()
}

// Close flushes and closes the file.
func (r *Recorder) Close() error {
	if err := r.Flush(); err != nil {
		_ = r.f.Close()
		return err
	}
	return r.f.Close()
}

// MaxRecordLine bounds one line of a recording.
const MaxRecordLine = 1 << 20

// ReadRecording decodes a JSON-lines recording. Invalid lines are reported
// in the returned count and skipped.
func ReadRecording(rd io.Reader) ([]obs.Record, int, error) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), MaxRecordLine)
	var out []obs.Record
	bad := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r obs.Record
		if err := json.Unmarshal(line, &r); err != nil {
			bad++
			continue
		}
		if store.Validate(r) != nil {
			bad++
			continue
		}
		out = append(out, r)
	}
	return out, bad, sc.Err()
}

// FlushEvery flushes the recording periodically until ctx is cancelled so
// that a running collector's recording can be read for replay.
func (r *Recorder) FlushEvery(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Flush()
		}
	}
}
