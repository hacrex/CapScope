package access

import (
	"strings"
	"testing"
)

func TestParseCombined(t *testing.T) {
	in := `203.0.113.42 - alice [29/Oct/2026:10:12:03 +0000] "GET /api/items?q=1 HTTP/1.1" 200 1534 "https://example.com/page\"one\"" "Mozilla/5.0 (X11; Linux) Gecko"`
	r, ok := Parse(in)
	if !ok {
		t.Fatal("combined line did not parse")
	}
	if r.IP != "203.0.113.42" || r.User != "alice" || r.Method != "GET" || r.Path != "/api/items?q=1" || r.Proto != "HTTP/1.1" {
		t.Errorf("fields: %+v", r)
	}
	if r.Status != 200 || r.Bytes != 1534 {
		t.Errorf("status/bytes: %+v", r)
	}
	if r.Referer != `https://example.com/page"one"` || r.UA != "Mozilla/5.0 (X11; Linux) Gecko" {
		t.Errorf("refer/ua: %q %q", r.Referer, r.UA)
	}
}

func TestParseCommonAndEdges(t *testing.T) {
	r, ok := Parse(`10.0.0.5 - - [29/Oct/2026:10:12:03 +0000] "POST /login HTTP/2.0" 404 -`)
	if !ok {
		t.Fatal("common line did not parse")
	}
	if r.User != "-" || r.Status != 404 || r.Bytes != 0 || r.Referer != "" || r.UA != "" {
		t.Errorf("common: %+v", r)
	}
	// "-" request and byte count, extended trailing fields still parse
	r, ok = Parse(`192.168.0.1 - - [29/Oct/2026:10:12:03 +0000] "-" 400 - "-" "-" 0.123 1.456 upstream`)
	if !ok || r.Status != 400 || r.Path != "-" {
		t.Errorf("malformed request with trailing fields: %+v %v", r, ok)
	}
	// IPv6 client
	r, ok = Parse(`2001:db8:85a3:0:0:8a2e:370:7334 - - [29/Oct/2026:10:12:03 +0000] "GET / HTTP/1.0" 200 42 "-" "-"`)
	if !ok || !strings.HasPrefix(r.IP, "2001:db8") {
		t.Errorf("ipv6: %+v %v", r, ok)
	}
	// not access logs
	for _, bad := range []string{
		"",
		"hello world",
		`{"level":"info","msg":"hi"}`,
		`[29/Oct/2026] GET / 200`,
		`1.2.3.4 - - [broken "request] 999 abc - "x" "y"`,
		`1.2.3.4 - - [29/Oct/2026:10:12:03 +0000] "GET / HTTP/1.1" 20- 5 "-" "-"`,
		"2026-10-28T08:09:58.000000001Z INFO listening on :3000",
	} {
		if _, ok := Parse(bad); ok {
			t.Errorf("line parsed as access log but should not: %q", bad)
		}
	}
}

func TestAnonymize(t *testing.T) {
	cases := [][2]string{
		{"203.0.113.42", "203.0.113.0"},
		{"10.1.2.3", "10.1.2.0"},
		{"2001:db8:85a3:1111:2222:3333:4444:5555", "2001:db8:85a3::"},
		{"not-an-ip", "not-an-ip"},
		{"-", "-"},
	}
	for _, c := range cases {
		if got := Anonymize(c[0]); got != c[1] {
			t.Errorf("Anonymize(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestDetectorAuto(t *testing.T) {
	line := `203.0.113.42 - - [29/Oct/2026:10:12:03 +0000] "GET / HTTP/1.1" 200 42 "-" "-"`
	line2 := "log message from an app"

	// proxy-named app: on from the first line
	s := NewDetector("auto").Stream("nginx")
	if _, ok := s.Observe(line); !ok {
		t.Error("proxy app stream should parse immediately")
	}

	// application log container: never turns on
	s = NewDetector("auto").Stream("shop")
	for i := 0; i < probeMax; i++ {
		if _, ok := s.Observe(line2); ok {
			t.Fatal("app stream turned on despite no access lines")
		}
	}

	// mixed traffic app that produces access logs: starts after probeMin lines
	s = NewDetector("auto").Stream("shop")
	for i := 0; i < probeMin; i++ {
		if _, ok := s.Observe(line); ok {
			t.Fatalf("stream on too early (line %d)", i+1)
		}
	}
	if _, ok := s.Observe(line); !ok {
		t.Fatal("stream should have turned on after probe window")
	}
	if _, ok := s.Observe(line); !ok {
		t.Fatal("parses should continue once on")
	}
	// non-parsing lines are tolerated but produce no record
	if _, ok := s.Observe(line2); ok {
		t.Error("plain line should not yield a record")
	}

	// forced modes
	s = NewDetector("all").Stream("shop")
	if _, ok := s.Observe(line); !ok {
		t.Error("mode all must parse")
	}
	s = NewDetector("off").Stream("nginx")
	if _, ok := s.Observe(line); ok {
		t.Error("mode off must never parse")
	}
	if _, ok := s.Observe(line2); ok {
		t.Error("mode off must never parse (2)")
	}
}
