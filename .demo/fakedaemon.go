// Command fakedaemon is a minimal read-only Docker Engine API stub used for the
// local demo when no Docker daemon is available. Demo tool only — not shipped.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var start = time.Now().Add(-2 * time.Hour)

type ctr struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

func list(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	out := []ctr{
		{ID: "aaa111", Names: []string{"/srv-captain--shop.1.abc"}, Image: "shop:latest", State: "running", Status: "Up 2 hours", Labels: map[string]string{"caprover-app": "shop"}},
		{ID: "bbb222", Names: []string{"/srv-captain--blog.1.def"}, Image: "blog:latest", State: "running", Status: "Up 90 minutes", Labels: map[string]string{"caprover-app": "blog"}},
	}
	if all {
		out = append(out, ctr{ID: "ccc333", Names: []string{"/srv-captain--old.1.ghi"}, Image: "old:latest", State: "exited", Status: "Exited (1) 30 minutes ago", Labels: map[string]string{"caprover-app": "old"}})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func inspect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/containers/")
	id = strings.TrimSuffix(id, "/json")
	name, img, app := "srv-captain--shop.1.abc", "shop:latest", "shop"
	switch id {
	case "bbb222":
		name, img, app = "srv-captain--blog.1.def", "blog:latest", "blog"
	case "ccc333":
		name, img, app = "srv-captain--old.1.ghi", "old:latest", "old"
	}
	o := map[string]any{
		"Id":   id,
		"Name": "/" + name,
		"Config": map[string]any{"Image": img, "Tty": false,
			"Labels": map[string]string{"caprover-app": app},
			"Env":    []string{}},
		"State": map[string]any{"Status": "running", "Running": id != "ccc333",
			"OOMKilled": false, "ExitCode": 0,
			"StartedAt":  start.Format(time.RFC3339Nano),
			"FinishedAt": "0001-01-01T00:00:00Z"},
		"HostConfig": map[string]any{"Memory": int64(536870912), "NanoCpus": int64(500000000),
			"RestartPolicy": map[string]string{"Name": "on-failure"}},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(o)
}

func bigEndianPutUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func logs(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/octet-stream")
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-tick.C:
			n++
			msgs := []string{
				fmt.Sprintf(`{"level":"info","msg":"request handled","path":"/api/items/%d","ms":%d}`, n%50, 5+n%30),
				"WARN slow query 1200ms",
				`{"level":"info","msg":"health check ok"}`,
			}
			line := msgs[n%len(msgs)]
			payload := t.Format(time.RFC3339Nano) + " " + line + "\n"
			hdr := make([]byte, 8)
			hdr[0] = 1 // stdout stream
			bigEndianPutUint32(hdr[4:], uint32(len(payload)))
			w.Write(hdr)
			w.Write([]byte(payload))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func stats(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	var cpu, sys, rx, tx uint64 = 0, 0, 1e6, 2e6
	for i := 0; ; i++ {
		if r.Context().Err() != nil {
			return
		}
		cpu += 4e6 + uint64(i%7)*1e5
		sys += 4e7
		rx += 9000
		tx += 4000
		s := map[string]any{
			"read": time.Now().Format(time.RFC3339Nano),
			"cpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": cpu, "percpu_usage": []uint64{cpu / 2, cpu / 2}},
				"system_cpu_usage": sys, "online_cpus": 2,
			},
			"precpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": cpu - 4e6, "percpu_usage": []uint64{(cpu - 4e6) / 2, (cpu - 4e6) / 2}},
				"system_cpu_usage": sys - 4e7, "online_cpus": 2,
			},
			"memory_stats": map[string]any{"usage": uint64(120<<20 + (i%20)<<18), "limit": uint64(512 << 20), "stats": map[string]uint64{"inactive_file": 20 << 20}},
			"networks":     map[string]any{"eth0": map[string]uint64{"rx_bytes": rx, "tx_bytes": tx}},
			"blkio_stats": map[string]any{"io_service_bytes_recursive": []map[string]any{
				{"op": "read", "value": uint64(i) * 1e5}, {"op": "write", "value": uint64(i) * 5e4}}},
			"pids_stats": map[string]uint64{"current": 12},
		}
		if err := enc.Encode(s); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(2 * time.Second)
	}
}

func events(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	ev := map[string]any{
		"Type": "container", "Action": "die", "status": "die",
		"timeNano": float64(time.Now().UnixNano()),
		"Actor": map[string]any{"ID": "aaa111", "Attributes": map[string]string{
			"name": "srv-captain--shop.1.abc", "image": "shop:latest",
			"exitCode": "137", "oomKilled": "true"}},
	}
	select {
	case <-time.After(25 * time.Second):
	case <-r.Context().Done():
		return
	}
	if err := enc.Encode(ev); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	select {}
}

func main() {
	sock := os.Args[1]
	os.Remove(sock)
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OK") })
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ApiVersion":"1.43"}`)
	})
	mux.HandleFunc("/containers/json", list)
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			inspect(w, r)
		case strings.Contains(r.URL.Path, "/logs"):
			logs(w, r)
		case strings.Contains(r.URL.Path, "/stats"):
			stats(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/events", events)
	l, err := net.Listen("unix", sock)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "fakedaemon listening on", sock)
	http.Serve(l, mux)
}
