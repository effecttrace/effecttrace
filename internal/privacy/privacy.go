// Package privacy implements EffectTrace's data-minimization defaults:
// identities of human users are pseudonymized, free text is sanitized and
// bounded, and user agents are reduced to their product token.
package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// IdentityMode controls how Kubernetes usernames are stored.
type IdentityMode string

const (
	// IdentityPseudonymize keeps system identities (system:*) verbatim and
	// replaces other usernames with a keyed hash. This is the default.
	IdentityPseudonymize IdentityMode = "pseudonymize"
	// IdentityKeep stores usernames verbatim.
	IdentityKeep IdentityMode = "keep"
)

// Policy applies privacy rules.
type Policy struct {
	Mode IdentityMode
	// Key keys the pseudonymization HMAC so that pseudonyms cannot be
	// reversed by hashing candidate usernames. An empty key still produces
	// stable pseudonyms but offers only weak protection.
	Key []byte
}

// Actor returns the stored form of a username.
func (p Policy) Actor(username string) string {
	if username == "" {
		return ""
	}
	if p.Mode == IdentityKeep || strings.HasPrefix(username, "system:") {
		return username
	}
	m := hmac.New(sha256.New, p.Key)
	m.Write([]byte(username))
	return "user:" + hex.EncodeToString(m.Sum(nil))[:12]
}

// UserAgent reduces a user agent to its first product token, bounded to 64
// bytes. "kubectl/v1.34.1 (darwin/arm64) kubernetes/abc" becomes
// "kubectl/v1.34.1".
func UserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if i := strings.IndexByte(ua, ' '); i >= 0 {
		ua = ua[:i]
	}
	return Text(ua, 64)
}

// Text replaces control characters with spaces, collapses whitespace and
// truncates to max bytes on a rune boundary.
func Text(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
