// Command capture-proxy is a development fake for the agent data-plane proof.
// It stands between an InferCrane agent and its control plane, terminates
// TLS, forwards every request unchanged, and keeps a copy of every request
// body the agent sent (decompressed), so a test can prove what left the
// customer's cloud. It never runs in production images.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const maxCaptured = 256 << 20

type capture struct {
	mu     sync.Mutex
	bodies bytes.Buffer
	paths  map[string]int
	bytes  int
}

func (c *capture) add(method, path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths[method+" "+path]++
	c.bytes += len(body)
	if c.bodies.Len()+len(body) > maxCaptured {
		return
	}
	c.bodies.WriteString(method + " " + path + "\n")
	c.bodies.Write(body)
	c.bodies.WriteString("\n")
}

func main() {
	listen := flag.String("listen", ":8443", "TLS listen address for the agent")
	report := flag.String("report", ":9090", "plain HTTP address that serves what was captured")
	upstream := flag.String("upstream", "http://infercrane:8080", "control plane URL")
	cert := flag.String("cert", "/tls/tls.crt", "TLS certificate")
	key := flag.String("key", "/tls/tls.key", "TLS key")
	flag.Parse()
	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	captured := &capture{paths: map[string]int{}}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	// outageUntil makes the proxy stand in for a control plane outage: the
	// agent's calls are refused until then (POST /outage?seconds=N).
	var outageMu sync.Mutex
	var outageUntil time.Time
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outageMu.Lock()
		down := time.Now().Before(outageUntil)
		outageMu.Unlock()
		if down {
			http.Error(w, "control plane outage (test)", http.StatusServiceUnavailable)
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if readErr != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			if reader, gzipErr := gzip.NewReader(bytes.NewReader(raw)); gzipErr == nil {
				if inflated, inflateErr := io.ReadAll(io.LimitReader(reader, 64<<20)); inflateErr == nil {
					body = inflated
				}
			}
		}
		// The query string is part of what left the cluster too.
		captured.add(r.Method, r.URL.RequestURI(), body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		proxy.ServeHTTP(w, r)
	})
	reportMux := http.NewServeMux()
	reportMux.HandleFunc("GET /captured", func(w http.ResponseWriter, _ *http.Request) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		_, _ = w.Write(captured.bodies.Bytes())
	})
	reportMux.HandleFunc("POST /outage", func(w http.ResponseWriter, r *http.Request) {
		seconds, err := strconv.Atoi(r.URL.Query().Get("seconds"))
		if err != nil || seconds < 0 || seconds > 3600 {
			http.Error(w, "seconds must be 0..3600", http.StatusBadRequest)
			return
		}
		outageMu.Lock()
		outageUntil = time.Now().Add(time.Duration(seconds) * time.Second)
		outageMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	reportMux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": captured.paths, "bytes": captured.bytes})
	})
	go func() {
		server := &http.Server{Addr: *report, Handler: reportMux, ReadHeaderTimeout: 10 * time.Second}
		log.Fatal(server.ListenAndServe())
	}()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("capturing %s -> %s", *listen, *upstream)
	log.Fatal(server.ListenAndServeTLS(*cert, *key))
}
