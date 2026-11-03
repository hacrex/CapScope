// Package logproc holds per-line processing: secret redaction and level detection.
package logproc

import (
	"regexp"
	"strconv"
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
	// JSON level fields as strings (level, lvl, severity, and OTel's
	// severityText; the otel collector often writes "severityText":"ERROR").
	jsonLevel = regexp.MustCompile(`"(?:level|lvl|severity|severityText)"\s*:\s*"([A-Za-z]+)"`)
	// Pino numeric levels: {"level":30} (20 debug, 30 info, 40 warn, 50 error,
	// 60 fatal). Useless outside 10..60 falls back to other heuristics.
	pinoNum = regexp.MustCompile(`"(?:level|lvl)"\s*:\s*(\d{1,2})\b`)
	// OpenTelemetry severityNumber: {"severityNumber":21} — a 1..24 scale,
	// four values per level band.
	otelNum = regexp.MustCompile(`"severityNumber"\s*:\s*(\d{1,2})\b`)
	wordLevel = regexp.MustCompile(`(?i)\b(FATAL|PANIC|CRITICAL|ERROR|ERR|WARN|WARNING|INFO|DEBUG|TRACE)\b`)
)

// Level returns one of "error", "warn", "info", "debug", or "" when unknown.
// It is a heuristic: JSON level fields first (string, then numeric Pino /
// OpenTelemetry), then a level word near the line start.
func Level(line string) string {
	if m := jsonLevel.FindStringSubmatch(line); m != nil {
		if l := normalize(m[1]); l != "" {
			return l
		}
	}
	if m := pinoNum.FindStringSubmatch(line); m != nil {
		if l := pinoLevel(m[1]); l != "" {
			return l
		}
	}
	if m := otelNum.FindStringSubmatch(line); m != nil {
		if l := otelLevel(m[1]); l != "" {
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

// pinoLevel maps Pino's numeric level (10..60) the same as Pino itself.
func pinoLevel(s string) string {
	switch s {
	case "10", "11": // trace
		return "debug"
	case "20":
		return "debug"
	case "30":
		return "info"
	case "40":
		return "warn"
	case "50", "60": // 60 = fatal
		return "error"
	}
	return ""
}

// otelLevel maps an OpenTelemetry severityNumber (1..24, four values per band
// per the spec: trace 1-4, debug 5-8, info 9-12, warn 13-16, error 17-20,
// fatal 21-24). Out-of-range values are not a level.
func otelLevel(s string) string {
	n, err := strconv.Atoi(s)
	if err != nil {
		return ""
	}
	switch {
	case n >= 1 && n <= 8:
		return "debug"
	case n >= 9 && n <= 12:
		return "info"
	case n >= 13 && n <= 16:
		return "warn"
	case n >= 17 && n <= 24:
		return "error"
	}
	return ""
}
