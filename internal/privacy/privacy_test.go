package privacy

import (
	"strings"
	"testing"
	"unicode"
)

func TestActorPseudonymizesHumansOnly(t *testing.T) {
	p := Policy{Mode: IdentityPseudonymize, Key: []byte("k")}
	if got := p.Actor("system:serviceaccount:shop:ci"); got != "system:serviceaccount:shop:ci" {
		t.Errorf("system identity changed: %q", got)
	}
	a, b := p.Actor("alice@example.com"), p.Actor("alice@example.com")
	if a != b || !strings.HasPrefix(a, "user:") || strings.Contains(a, "alice") {
		t.Errorf("pseudonym %q / %q", a, b)
	}
	if (Policy{Mode: IdentityPseudonymize, Key: []byte("other")}).Actor("alice@example.com") == a {
		t.Error("pseudonym does not depend on the key")
	}
	if (Policy{Mode: IdentityKeep}).Actor("alice") != "alice" {
		t.Error("keep mode altered the username")
	}
}

func TestUserAgentAndText(t *testing.T) {
	if got := UserAgent("kubectl/v1.34.1 (darwin/arm64) kubernetes/93248f9"); got != "kubectl/v1.34.1" {
		t.Errorf("UserAgent = %q", got)
	}
	if got := Text("a\x1b[2J\n\tb  c", 100); got != "a [2J b c" {
		t.Errorf("Text = %q", got)
	}
	if got := Text(strings.Repeat("é", 100), 11); len(got) > 11 {
		t.Errorf("Text exceeded bound: %d bytes", len(got))
	}
}

func FuzzText(f *testing.F) {
	f.Add("Scaled up replica set \x1b]0;pwned\x07 to 3", 64)
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max < 0 || max > 4096 {
			return
		}
		out := Text(s, max)
		if len(out) > max {
			t.Fatalf("len %d > %d", len(out), max)
		}
		for _, r := range out {
			if unicode.IsControl(r) {
				t.Fatalf("control character %U in output", r)
			}
		}
	})
}

func TestRandomKey(t *testing.T) {
	a, err := RandomKey()
	if err != nil || len(a) != 32 {
		t.Fatal(err)
	}
	b, _ := RandomKey()
	if string(a) == string(b) {
		t.Fatal("keys repeat")
	}
}
