package logproc

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	secrets := []string{
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk123",
		"s3cr3tPassw0rd",
		"AKIAABCDEFGHIJKLMNOP",
		"sup3rtokenvalue123",
		"hunter2hunter2",
	}
	lines := []string{
		"login ok token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk123",
		`connecting {"password":"s3cr3tPassw0rd","user":"bob"}`,
		"aws key AKIAABCDEFGHIJKLMNOP in use",
		"Authorization: Bearer sup3rtokenvalue123",
		"dial postgres://app:hunter2hunter2@db:5432/app",
	}
	for i, l := range lines {
		out := Redact(l)
		if strings.Contains(out, secrets[i]) {
			t.Errorf("secret leaked: %q -> %q", l, out)
		}
	}
	if got := Redact("GET /health 200 12ms"); got != "GET /health 200 12ms" {
		t.Errorf("clean line changed: %q", got)
	}
	if got := Redact("user=bob action=login"); got != "user=bob action=login" {
		t.Errorf("clean kv changed: %q", got)
	}
}

func TestLevel(t *testing.T) {
	cases := map[string]string{
		`{"level":"error","msg":"boom"}`:            "error",
		`{"lvl":"WARNING","msg":"x"}`:               "warn",
		"2026-10-28 ERROR db connection refused":    "error",
		"[warn] upstream slow":                      "warn",
		"level=info msg=started":                    "info",
		"DEBUG cache miss":                          "debug",
		"panic: runtime error: index out of range":  "error",
		"GET /index.html 200":                       "",
		"Traceback (most recent call last):":        "error",
		"ERR_CONNECTION is just a token, not level": "",
	}
	for in, want := range cases {
		if got := Level(in); got != want {
			t.Errorf("Level(%q) = %q, want %q", in, got, want)
		}
	}
}
