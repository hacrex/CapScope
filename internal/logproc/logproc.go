// Package logproc holds per-line processing: secret redaction and level detection.
package logproc

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

type redactor struct {
	name string // human-readable shape name, used by the redaction audit endpoint
	re   *regexp.Regexp
	repl string
}

var redactors = []redactor{
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z ]*PRIVATE KEY|OPENSSH PRIVATE KEY)-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|-----END OPENSSH PRIVATE KEY-----|$)`), "[REDACTED_PRIVATE_KEY]"},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED_JWT]"},
	{"github_token", regexp.MustCompile(`(?:ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{20,})`), "[REDACTED_GITHUB_TOKEN]"},
	{"stripe_key", regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{24,}\b`), "[REDACTED_STRIPE_KEY]"},
	{"slack_webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Za-z0-9_]{8,}/B[A-Za-z0-9_]{8,}/[A-Za-z0-9]{24,}`), "[REDACTED_SLACK_WEBHOOK]"},
	{"bearer_token", regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} [REDACTED]"},
	{"api_key_header", regexp.MustCompile(`(?i)(?:X-API-Key|Authorization:\s*(?:Apikey|API-Key|Token))\s*[:]\s*[A-Za-z0-9._~+/=-]{8,}`), "$1 [REDACTED]"},
	{"aws_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED_AWS_KEY]"},
	{"url_credential", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), "${1}[REDACTED]@"},
	{"kv_secret", regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|authorization|private[_-]?key|client[_-]?secret|signing[_-]?key|encryption[_-]?key)["']?\s*[:=]\s*["']?)([^\s"',;&}\]]+)`), "${1}[REDACTED]"},
}

// Redact masks common secret shapes (JWTs, bearer tokens, key=value secrets,
// URL credentials, AWS keys, PEM private keys). It is best effort, not a guarantee.
func Redact(line string) string {
	for _, r := range redactors {
		line = r.re.ReplaceAllString(line, r.repl)
	}
	return line
}

// LeakedShapes reports which secret shapes still appear in the redacted
// output — used by the /admin/redact-test audit endpoint. Returns shape
// names whose regex matches the redacted line (i.e. redaction failed for
// that shape).
func LeakedShapes(line string) []string {
	redacted := Redact(line)
	var leaked []string
	for _, r := range redactors {
		if r.re.MatchString(redacted) {
			leaked = append(leaked, r.name)
		}
	}
	return leaked
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

// Template fingerprinting for semantic log clustering.
//
// Template() normalizes a log line by replacing variable parts (IPs, numbers,
// UUIDs, hex strings, quoted values) with stable placeholders. Lines that
// share the same template are likely the same recurring log message with
// different data, so they can be clustered together for anomaly detection
// and noise reduction.
//
// The template is deterministic: the same input always produces the same
// output, which makes it safe to use as a grouping key in SQLite.
func Template(line string) string {
	t := line

	// UUIDs: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	t = uuidRe.ReplaceAllString(t, "<uuid>")

	// IPv4 addresses
	t = ipv4Re.ReplaceAllString(t, "<ip>")

	// Hex strings (6+ chars): hashes, object IDs, colors
	t = hexRe.ReplaceAllString(t, "<hex>")

	// Numbers (standalone, not part of words): replace digit sequences
	// with <num>. We do this after UUID/IP/hex so those are already masked.
	t = numRe.ReplaceAllString(t, "<num>")

	// Quoted string values: "value" -> "<str>" (but not JSON keys)
	t = quotedRe.ReplaceAllString(t, "<str>")

	// Collapse multiple whitespace into single space
	t = whitespaceRe.ReplaceAllString(t, " ")

	return strings.TrimSpace(t)
}

var (
	// uuidRe matches standard UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	uuidRe = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// ipv4Re matches IPv4 addresses: 1.2.3.4
	ipv4Re = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`)

	// hexRe matches hex strings of 6+ characters (hashes, colors, IDs)
	hexRe = regexp.MustCompile(`\b[0-9a-fA-F]{6,}\b`)

	// numRe matches standalone digit sequences (not part of larger words).
	// UUIDs, IPs, and hex strings are replaced before this runs, so remaining
	// digit sequences are typically version numbers, port numbers, etc.
	numRe = regexp.MustCompile(`\b[0-9]+\b`)

	// quotedRe matches double-quoted string values
	quotedRe = regexp.MustCompile(`"[^"]*"`)

	// whitespaceRe matches runs of whitespace
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// TemplateHash returns a stable SHA256-based hash of the template for use
// as a database grouping key. It returns the first 16 hex chars (64 bits),
// which is more than enough for clustering within a single database.
func TemplateHash(line string) string {
	t := Template(line)
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])[:16]
}
