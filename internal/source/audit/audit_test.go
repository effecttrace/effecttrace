package audit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/privacy"
)

func TestParseFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var got []*obs.AuditRequest
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		r, err := Parse(line, privacy.Policy{Mode: privacy.IdentityPseudonymize, Key: []byte("k")})
		if errors.Is(err, ErrSkip) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d mutating ResponseComplete events, want 2", len(got))
	}
	a := got[0]
	if a.Verb != "patch" || a.Resource != "deployments" || a.Name != "checkout" || a.StatusCode != http.StatusOK {
		t.Errorf("unexpected request: %+v", a)
	}
	if a.User == "kubernetes-admin" || len(a.User) != len("user:")+12 {
		t.Errorf("human user not pseudonymized: %q", a.User)
	}
	if a.UserAgent != "kubectl/v1.34.1" {
		t.Errorf("user agent = %q", a.UserAgent)
	}
	if !a.ReceivedAt.Equal(time.Date(2026, 10, 8, 10, 0, 0, 123456000, time.UTC)) {
		t.Errorf("received = %v", a.ReceivedAt)
	}
	b := got[1]
	if !b.DryRun || b.Subresource != "scale" || b.User != "system:serviceaccount:effecttrace-demo:demo-actor" {
		t.Errorf("unexpected second request: %+v", b)
	}
}

func TestParseRejectsForeignJSON(t *testing.T) {
	for _, in := range []string{`{}`, `{"kind":"Pod"}`, `not json`, `{"kind":"Event","apiVersion":"v1"}`} {
		if _, err := Parse([]byte(in), privacy.Policy{}); err == nil || errors.Is(err, ErrSkip) {
			t.Errorf("Parse(%q) = %v, want decode error", in, err)
		}
	}
}

func TestLineReaderCarriesPartialAndSkipsOverlong(t *testing.T) {
	var buf bytes.Buffer
	lr := &lineReader{rd: bufio.NewReaderSize(&buf, 16)}
	buf.WriteString("abc")
	if _, _, err := lr.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("partial line: err = %v, want EOF", err)
	}
	buf.WriteString("def\n")
	var line []byte
	for {
		l, _, err := lr.next()
		if errors.Is(err, errMore) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		line = l
		break
	}
	if string(line) != "abcdef\n" {
		t.Fatalf("line = %q", line)
	}
	buf.Write(bytes.Repeat([]byte("x"), MaxLineBytes+10))
	buf.WriteString("\nok\n")
	skipped := false
	for {
		l, _, err := lr.next()
		if errors.Is(err, errMore) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			skipped = true
			continue
		}
		if string(l) != "ok\n" {
			t.Fatalf("after overlong: %q", l)
		}
		break
	}
	if !skipped {
		t.Fatal("overlong line not reported as skipped")
	}
}

func TestTailerFollowsAppendsAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	fixture, _ := os.ReadFile("testdata/audit.jsonl")
	lines := bytes.Split(bytes.TrimSpace(fixture), []byte("\n"))
	if err := os.WriteFile(path, append(lines[1], '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ids []string
	got := make(chan struct{}, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tl := &Tailer{Path: path, Poll: 10 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), Sink: func(_ context.Context, r obs.Record) error {
		if r.Kind == obs.KindAudit {
			mu.Lock()
			ids = append(ids, r.Audit.AuditID)
			mu.Unlock()
			got <- struct{}{}
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()
	wait := func() {
		select {
		case <-got:
		case <-ctx.Done():
			t.Fatal("timed out waiting for audit record")
		}
	}
	wait()
	// Rotate: rename and write a new file.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(lines[3], '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	wait()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("ids = %v", ids)
	}
}

func FuzzParse(f *testing.F) {
	data, _ := os.ReadFile("testdata/audit.jsonl")
	for _, line := range bytes.Split(data, []byte("\n")) {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		r, err := Parse(line, privacy.Policy{Mode: privacy.IdentityPseudonymize})
		if err != nil {
			return
		}
		if r.Verb == "" || r.AuditID == "" {
			t.Fatal("accepted audit event without verb or ID")
		}
		if r.User != "" && !strings.HasPrefix(r.User, "system:") && !strings.HasPrefix(r.User, "user:") {
			t.Fatalf("human username stored verbatim: %q", r.User)
		}
	})
}
