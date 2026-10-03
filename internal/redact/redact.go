// Package redact removes secrets from any text before it is logged or
// posted to Slack.
package redact

import (
	"math"
	"regexp"
	"strings"
)

const mask = "[REDACTED]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]{6,}`),                       // Slack bot/user/app/legacy tokens
	regexp.MustCompile(`xoxe[.-][A-Za-z0-9.-]{6,}`),                       // Slack refresh / config tokens
	regexp.MustCompile(`xapp-[A-Za-z0-9-]{6,}`),                           // Slack app-level tokens
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),                        // Anthropic
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                           // OpenAI & co (incl. sk-proj-)
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                      // GitHub classic
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                    // GitHub fine-grained
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                // AWS access key id
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`),                          // Google API key
	regexp.MustCompile(`plx1\.[a-z0-9]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), // Plexus task tokens
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWT
}

// key=value style assignments of secret-looking names keep the name.
var assignment = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:api[_-]?key|secret|token|passwd|password|private[_-]?key|client[_-]?secret)[A-Za-z0-9_]*)(\s*[:=]\s*["']?)([^\s"']{8,})`)

// candidate generic high-entropy strings.
var blob = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)

// String redacts s. Extra exact secrets (e.g. the live bot tokens) are
// replaced first.
func String(s string, extra ...string) string {
	for _, e := range extra {
		if len(e) >= 8 {
			s = strings.ReplaceAll(s, e, mask)
		}
	}
	for _, p := range patterns {
		s = p.ReplaceAllString(s, mask)
	}
	s = assignment.ReplaceAllString(s, "${1}${2}"+mask)
	s = blob.ReplaceAllStringFunc(s, func(m string) string {
		if looksLikeKey(m) {
			return mask
		}
		return m
	})
	return s
}

// looksLikeKey: mixed upper, lower and digits with high Shannon entropy.
// Hex digests (git SHAs) and ordinary identifiers do not qualify.
func looksLikeKey(s string) bool {
	var up, lo, dg bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			lo = true
		case r >= '0' && r <= '9':
			dg = true
		}
	}
	return up && lo && dg && entropy(s) >= 4.2
}

func entropy(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
