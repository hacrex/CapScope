// Package access parses web-server access logs (nginx combined / common
// format, the default on CapRover's captain-nginx and most proxies) and
// decides which containers emit them.
package access

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Record is one parsed request. Fields match the parts of the stored
// `access` table row; ts stays the Docker log timestamp (the collector
// supplies it), so the format's own time is only validated, not parsed.
type Record struct {
	IP, User, Method, Path, Proto string
	Status                        int
	Bytes                         int64
	Referer, UA                   string
}

var (
	// nginx "combined": ip ident user [time] "request" status bytes "referer" "ua"
	// A trailing remainder is allowed so extended formats (request_time, upstream
	// time, Traefik's CLF flavor) still parse.
	reCombined = regexp.MustCompile(`^(\S+) (\S+) (\S+) \[([^\]]*)\] "((?:\\.|[^"\\])*)" (\d{3}) (\d+|-) "((?:\\.|[^"\\])*)" "((?:\\.|[^"\\])*)"(?: +.*)?$`)
	// nginx "common": the same without referer and user agent.
	reCommon = regexp.MustCompile(`^(\S+) (\S+) (\S+) \[([^\]]*)\] "((?:\\.|[^"\\])*)" (\d{3}) (\d+|-)(?: +.*)?$`)

	unescaping = strings.NewReplacer(`\"`, `"`, `\\`, `\`)
)

// Parse decodes one access-log line. It returns ok=false for anything that is
// not an access-log entry (UTF-8 garbage, structured JSON logs, errors).
func Parse(line string) (Record, bool) {
	s := strings.TrimRight(line, "\r\n \t")
	combined := true
	m := reCombined.FindStringSubmatch(s)
	if m == nil {
		m = reCommon.FindStringSubmatch(s)
		combined = false
	}
	if m == nil {
		return Record{}, false
	}
	status, _ := strconv.Atoi(m[6])
	var bytes int64
	if m[7] != "-" {
		bytes, _ = strconv.ParseInt(m[7], 10, 64)
	}
	method, path, proto := request(m[5])
	r := Record{
		IP: m[1], User: m[3],
		Method: method, Path: path, Proto: proto,
		Status: status, Bytes: bytes,
	}
	if combined {
		r.Referer, r.UA = unescaping.Replace(m[8]), unescaping.Replace(m[9])
	}
	return r, true
}

// request splits `"GET /p?q=1 HTTP/1.1"` into its parts. Empty and "-"
// requests (nginx logs these on malformed input) are kept as-is.
func request(req string) (method, path, proto string) {
	f := strings.Fields(req)
	if len(f) > 0 {
		method = f[0]
	}
	switch len(f) {
	case 0, 1:
		return method, strings.Join(f, " "), ""
	case 2:
		return method, f[1], ""
	default:
		if strings.HasPrefix(f[len(f)-1], "HTTP/") {
			return method, strings.Join(f[1:len(f)-1], " "), f[len(f)-1]
		}
		return method, strings.Join(f[1:], " "), ""
	}
}

// Anonymize replaces the host-identifying bits of an address: the last octet
// of IPv4 addresses is zeroed (192.168.1.42 -> 192.168.1.0), and IPv6
// addresses are cut to /48 like GoAccess's anonomization mode. Non-IP values
// pass through unchanged.
func Anonymize(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return p.Mask(net.CIDRMask(48, 128)).String()
}

var proxyName = regexp.MustCompile(`(?i)nginx|httpd|apache|caddy|traefik|haproxy|envoy|kong|proxy`)

// Detector decides per container whether its stdout is an access log.
//
//	mode "all":  try to parse every container's lines
//	mode "off":  never parse (pure application-log containers)
//	mode "auto": containers whose app name says reverse proxy are always on;
//	             everything else is briefly probed and switched on when most
//	             of a probe window parses, off (permanently) when it is
//	             clearly something else. Lines emitted before the switch are
//	             not back-filled into the access table.
type Detector struct{ mode string }

// NewDetector builds a detector for the ACCESS_LOG setting.
func NewDetector(mode string) Detector {
	switch mode {
	case "all", "off":
		return Detector{mode: mode}
	}
	return Detector{mode: "auto"}
}

// Stream is the per-container state. One per tracked container's stdout.
type Stream struct {
	on       bool
	seen     int
	hits     int
	resigned bool
}

func (d Detector) Stream(app string) *Stream {
	if d.mode == "all" {
		return &Stream{on: true}
	}
	if d.mode == "off" {
		return &Stream{resigned: true}
	}
	return &Stream{on: proxyName.MatchString(app)}
}

const (
	probeMin = 20
	probeMax = 100
	onRate   = 0.6
)

// Observe feeds one (already redacted) log line. A Record comes back when the
// stream is enabled and the line parses as an access-log entry.
func (s *Stream) Observe(line string) (Record, bool) {
	if s.resigned {
		return Record{}, false
	}
	if s.on {
		return Parse(line)
	}
	if s.seen++; s.seen > probeMax {
		s.resigned = true
		return Record{}, false
	}
	if _, ok := Parse(line); ok {
		s.hits++
	}
	if s.seen >= probeMin && float64(s.hits)/float64(s.seen) >= onRate {
		s.on = true
	}
	return Record{}, false
}
