package admin

import (
	"embed"
	"net/http"
)

// The console is a single self-contained HTML file with inline styles and
// script. Embedding it keeps deployment to one binary, and avoiding a frontend
// build step keeps the repository free of a toolchain.
//
//go:embed web/index.html
var uiFS embed.FS

func (a *Admin) serveUI(w http.ResponseWriter) {
	page, err := uiFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "console asset is missing from the binary", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}
