// Package audit reads kube-apiserver audit events (audit.k8s.io/v1, JSON
// lines, log backend) and reduces them to request metadata.
//
// Only ResponseComplete events are used. Request and response bodies,
// requestURI query parameters and source IPs are never retained.
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/privacy"
)

// MaxLineBytes bounds one audit line. Longer lines are skipped.
const MaxLineBytes = 1 << 20

// event is the subset of audit.k8s.io/v1 Event that EffectTrace reads.
type event struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	AuditID    string `json:"auditID"`
	Stage      string `json:"stage"`
	RequestURI string `json:"requestURI"`
	Verb       string `json:"verb"`
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	ImpersonatedUser *struct {
		Username string `json:"username"`
	} `json:"impersonatedUser"`
	UserAgent string `json:"userAgent"`
	ObjectRef *struct {
		Resource    string `json:"resource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		UID         string `json:"uid"`
		APIGroup    string `json:"apiGroup"`
		APIVersion  string `json:"apiVersion"`
		Subresource string `json:"subresource"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
	RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
	StageTimestamp           time.Time `json:"stageTimestamp"`
}

// ErrSkip marks lines that are valid but not used (other stages, reads).
var ErrSkip = errors.New("audit event skipped")

// Parse converts one audit JSON line into an AuditRequest.
func Parse(line []byte, p privacy.Policy) (*obs.AuditRequest, error) {
	var e event
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("decode audit event: %w", err)
	}
	if e.Kind != "Event" || !strings.HasPrefix(e.APIVersion, "audit.k8s.io/") {
		return nil, fmt.Errorf("not an audit event: kind=%q apiVersion=%q", e.Kind, e.APIVersion)
	}
	if e.Stage != "ResponseComplete" {
		return nil, ErrSkip
	}
	switch e.Verb {
	case "create", "update", "patch", "delete", "deletecollection":
	default:
		return nil, ErrSkip
	}
	if e.ObjectRef == nil || e.AuditID == "" {
		return nil, ErrSkip
	}
	user := e.User.Username
	if e.ImpersonatedUser != nil && e.ImpersonatedUser.Username != "" {
		user = e.ImpersonatedUser.Username
	}
	r := &obs.AuditRequest{
		AuditID:     e.AuditID,
		Verb:        e.Verb,
		APIGroup:    e.ObjectRef.APIGroup,
		APIVersion:  e.ObjectRef.APIVersion,
		Resource:    e.ObjectRef.Resource,
		Subresource: e.ObjectRef.Subresource,
		Namespace:   e.ObjectRef.Namespace,
		Name:        e.ObjectRef.Name,
		ObjectUID:   e.ObjectRef.UID,
		User:        p.Actor(user),
		UserAgent:   privacy.UserAgent(e.UserAgent),
		DryRun:      strings.Contains(e.RequestURI, "dryRun="),
		ReceivedAt:  e.RequestReceivedTimestamp.UTC(),
		CompletedAt: e.StageTimestamp.UTC(),
	}
	if e.ResponseStatus != nil {
		r.StatusCode = e.ResponseStatus.Code
	}
	return r, nil
}

// Sink receives records. It must block while the pipeline is full.
type Sink func(ctx context.Context, r obs.Record) error

// Tailer follows an audit log file, including rotation by kube-apiserver.
type Tailer struct {
	Path      string
	Policy    privacy.Policy
	Poll      time.Duration
	FromEnd   bool
	Logger    *slog.Logger
	Sink      Sink
	startedAt time.Time
}

// Run tails until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) error {
	if t.Poll <= 0 {
		t.Poll = 250 * time.Millisecond
	}
	t.startedAt = time.Now().UTC()
	ticker := time.NewTicker(t.Poll)
	defer ticker.Stop()
	status := time.NewTicker(10 * time.Second)
	defer status.Stop()

	var (
		f       *os.File
		lr      *lineReader
		offset  int64
		first   = true
		lastErr string
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	report := func(healthy bool, detail string) {
		_ = t.Sink(ctx, obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{
			Source: "kubernetes-audit", At: time.Now().UTC(), Healthy: healthy, Detail: detail, StartedAt: t.startedAt,
		}})
	}
	for {
		if f == nil {
			nf, err := os.Open(t.Path)
			if err != nil {
				if msg := "cannot open audit log: " + err.Error(); msg != lastErr {
					t.Logger.Warn("audit log unavailable", "err", err)
					report(false, "audit log unavailable")
					lastErr = msg
				}
			} else {
				f = nf
				offset = 0
				if first && t.FromEnd {
					if off, err := f.Seek(0, io.SeekEnd); err == nil {
						offset = off
					}
				}
				first = false
				lr = &lineReader{rd: bufio.NewReaderSize(f, 64<<10)}
				lastErr = ""
				report(true, "tailing audit log")
			}
		}
		if f != nil {
			n, err := t.drain(ctx, lr)
			offset += n
			if !errors.Is(err, io.EOF) {
				return err
			}
			if rotated(f, t.Path, offset) {
				_ = f.Close()
				f = nil
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-status.C:
			if f != nil {
				report(true, "tailing audit log")
			}
		case <-ticker.C:
		}
	}
}

// lineReader splits a growing file into lines. An incomplete trailing line
// is carried over to the next read; lines longer than MaxLineBytes are
// discarded up to their newline.
type lineReader struct {
	rd       *bufio.Reader
	pending  []byte
	skipping bool
}

// next returns the next complete line, or io.EOF when no complete line is
// available yet. A nil line with nil error means an overlong line was skipped.
func (l *lineReader) next() ([]byte, int, error) {
	frag, err := l.rd.ReadSlice('\n')
	n := len(frag)
	if !l.skipping {
		if len(l.pending)+len(frag) > MaxLineBytes {
			l.skipping = true
			l.pending = nil
		} else {
			l.pending = append(l.pending, frag...)
		}
	}
	switch {
	case err == nil:
		line := l.pending
		skipped := l.skipping
		l.pending, l.skipping = nil, false
		if skipped {
			return nil, n, nil
		}
		return line, n, nil
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, n, errMore
	default:
		return nil, n, err
	}
}

var errMore = errors.New("line continues")

// drain processes complete lines until no complete line is available.
func (t *Tailer) drain(ctx context.Context, lr *lineReader) (int64, error) {
	var read int64
	for {
		line, n, err := lr.next()
		read += int64(n)
		if errors.Is(err, errMore) {
			continue
		}
		if err != nil {
			return read, err
		}
		if line == nil {
			t.Logger.Debug("skipped overlong audit line")
			continue
		}
		req, perr := Parse(line, t.Policy)
		if perr != nil {
			if !errors.Is(perr, ErrSkip) {
				t.Logger.Debug("rejected audit line", "err", perr)
			}
			continue
		}
		if err := t.Sink(ctx, obs.Record{Kind: obs.KindAudit, Audit: req}); err != nil {
			return read, err
		}
	}
}

func rotated(f *os.File, path string, offset int64) bool {
	cur, err := f.Stat()
	if err != nil {
		return true
	}
	disk, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !os.SameFile(cur, disk) || disk.Size() < offset
}
