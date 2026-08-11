// Package dashboard serves the embedded operational evidence client. The
// browser application is intentionally static: every datum and mutation flows
// through the authenticated public control-plane API.
package dashboard

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*
var assets embed.FS

type handler struct{ files fs.FS }

// Handler returns the release-embedded dashboard handler.
func Handler() http.Handler {
	files, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return handler{files: files}
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/dashboard" {
		http.Redirect(w, r, "/dashboard/", http.StatusPermanentRedirect)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/dashboard/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/dashboard/")
	if name == "" {
		name = "index.html"
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || strings.HasPrefix(clean, "../") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(h.files, clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(clean))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func securityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
