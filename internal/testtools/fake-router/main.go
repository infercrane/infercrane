package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type stringsFlag []string

func (s *stringsFlag) String() string     { return fmt.Sprint(*s) }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func main() {
	host := flag.String("host", "127.0.0.1", "host")
	port := flag.Int("port", 18081, "port")
	var workers stringsFlag
	flag.Var(&workers, "worker-urls", "workers")
	flag.String("policy", "round_robin", "policy")
	flag.String("api-key", "", "key")
	flag.String("retry-max-retries", "1", "retries")
	flag.Parse()
	if len(workers) == 0 {
		panic("worker URLs required")
	}
	proxies := make([]*httputil.ReverseProxy, len(workers))
	for i, worker := range workers {
		target, err := url.Parse(worker)
		if err != nil {
			panic(err)
		}
		proxies[i] = httputil.NewSingleHostReverseProxy(target)
	}
	var next atomic.Uint64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxies[(next.Add(1)-1)%uint64(len(proxies))].ServeHTTP(w, r)
	})
	if err := http.ListenAndServe(fmt.Sprintf("%s:%d", *host, *port), mux); err != nil {
		panic(err)
	}
}
