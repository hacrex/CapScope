package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// runHealthcheck probes /healthz without a shell in the image (scratch).
// Exit 0 when healthy, 1 otherwise.
func runHealthcheck(log *slog.Logger) error {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0", "127.0.0.1", 1)
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return errors.New("healthcheck: cannot reach /healthz: " + err.Error())
	}
	defer io.Copy(io.Discard, r.Body)
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: /healthz returned %d", r.StatusCode)
	}
	log.Info("healthcheck ok", "addr", addr)
	return nil
}
