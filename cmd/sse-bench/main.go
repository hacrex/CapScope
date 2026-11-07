// Command sse-bench opens concurrent CapScope SSE clients and reports connection
// capacity, event delivery, errors, and optional target-process RSS.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/api/logs/stream?app=", "SSE endpoint")
	token := flag.String("token", "", "Bearer token")
	connections := flag.Int("connections", 100, "connections to open")
	step := flag.Int("step", 25, "connections opened per batch")
	duration := flag.Duration("duration", 20*time.Second, "soak duration after opening")
	pid := flag.Int("pid", 0, "optional CapScope server PID for VmRSS sampling")
	flag.Parse()
	if *connections <= 0 || *step <= 0 {
		fmt.Fprintln(os.Stderr, "connections and step must be positive")
		os.Exit(2)
	}

	client := &http.Client{Timeout: 0}
	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()
	var mu sync.Mutex
	responses := make([]*http.Response, 0, *connections)
	var opened, failed, events, errors atomic.Int64
	openStart := time.Now()
	for start := 0; start < *connections; start += *step {
		end := start + *step
		if end > *connections {
			end = *connections
		}
		for i := start; i < end; i++ {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
			if *token != "" {
				req.Header.Set("Authorization", "Bearer "+*token)
			}
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				failed.Add(1)
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			opened.Add(1)
			mu.Lock()
			responses = append(responses, resp)
			mu.Unlock()
			go func(body *http.Response) {
				sc := bufio.NewScanner(body.Body)
				for sc.Scan() {
					if strings.HasPrefix(sc.Text(), "data: ") {
						events.Add(1)
					}
				}
				if err := sc.Err(); err != nil {
					errors.Add(1)
				}
			}(resp)
		}
		fmt.Printf("opened=%d failed=%d rss=%s\n", opened.Load(), failed.Load(), rss(*pid))
	}
	soakStart := time.Now()
	ticker := time.NewTicker(time.Second)
	for time.Since(soakStart) < *duration {
		<-ticker.C
		fmt.Printf("soak elapsed=%s opened=%d failed=%d events=%d errors=%d rss=%s\n", time.Since(soakStart).Round(time.Second), opened.Load(), failed.Load(), events.Load(), errors.Load(), rss(*pid))
	}
	ticker.Stop()
	cancel()
	mu.Lock()
	for _, resp := range responses {
		_ = resp.Body.Close()
	}
	mu.Unlock()
	fmt.Printf("result opened=%d failed=%d events=%d read_errors=%d open_time=%s rss=%s\n", opened.Load(), failed.Load(), events.Load(), errors.Load(), time.Since(openStart).Round(time.Millisecond), rss(*pid))
}

func rss(pid int) string {
	if pid <= 0 {
		return "n/a"
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return "unavailable"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		}
	}
	return "unknown"
}
