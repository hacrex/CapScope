// Package logproc holds per-line processing: secret redaction and level detection.
package logproc

import (
	"regexp"
	"strings"
)

type redactor struct {
	re   *regexp.Regexp
	repl string
}

var redactors = []redactor{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED_PRIVATE_KEY]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED_JWT]"},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} [REDACTED]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED_AWS_KEY]"},
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), "${1}[REDACTED]@"},
	{regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|authorization|private[_-]?key)["']?\s*[:=]\s*["']?)([^\s"',;&}\]]+)`), "${1}[REDACTED]"},
}

// Redact masks common secret shapes (JWTs, bearer tokens, key=value secrets,
// URL credentials, AWS keys, PEM private keys). It is best effort, not a guarantee.
func Redact(line string) string {
	for _, r := range redactors {
		line = r.re.ReplaceAllString(line, r.repl)
	}
	return line
}

var (
	jsonLevel = regexp.MustCompile(`"(?:level|lvl|severity)"\s*:\s*"([A-Za-z]+)"`)
	wordLevel = regexp.MustCompile(`(?i)\b(FATAL|PANIC|CRITICAL|ERROR|ERR|WARN|WARNING|INFO|DEBUG|TRACE)\b`)
)

// Level returns one of "error", "warn", "info", "debug", or "" when unknown.
// It is a heuristic: JSON level fields first, then a level word near the line start.
func Level(line string) string {
	if m := jsonLevel.FindStringSubmatch(line); m != nil {
		if l := normalize(m[1]); l != "" {
			return l
		}
	}
	head := line
	if len(head) > 160 {
		head = head[:160]
	}
	if m := wordLevel.FindString(head); m != "" {
		return normalize(m)
	}
	if strings.Contains(head, "Traceback (most recent call last)") || strings.HasPrefix(line, "panic:") {
		return "error"
	}
	return ""
}

func normalize(s string) string {
	switch strings.ToLower(s) {
	case "fatal", "panic", "critical", "error", "err":
		return "error"
	case "warn", "warning":
		return "warn"
	case "info":
		return "info"
	case "debug", "trace":
		return "debug"
	}
	return ""
}
