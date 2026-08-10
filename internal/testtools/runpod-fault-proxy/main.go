package main

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	upstream, err := url.Parse(env("RUNPOD_PROXY_UPSTREAM", "https://rest.runpod.io"))
	if err != nil {
		log.Fatal(err)
	}
	dropCreate := strings.EqualFold(os.Getenv("RUNPOD_PROXY_DROP_FIRST_CREATE_RESPONSE"), "true") || os.Getenv("RUNPOD_PROXY_DROP_FIRST_CREATE_RESPONSE") == "1"
	var dropped atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = upstream.Host
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if dropCreate && response.Request.Method == http.MethodPost && response.Request.URL.Path == "/v1/endpoints" && response.StatusCode >= 200 && response.StatusCode < 300 && dropped.CompareAndSwap(false, true) {
			return errors.New("acceptance fault: provider create response intentionally lost")
		}
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
		log.Printf("RunPod acceptance proxy: %v", proxyErr)
		http.Error(writer, "provider response unavailable", http.StatusBadGateway)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.Handle("/", proxy)
	server := http.Server{Addr: env("RUNPOD_PROXY_ADDR", ":8090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
