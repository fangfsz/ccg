package main

import (
	"net/http"

	"ccg/internal/config"
	"ccg/internal/proxy"
	"ccg/internal/storage"
	"ccg/cmd/server/webui"
)

// registerWebUI registers the Web UI routes
func registerWebUI(mux *http.ServeMux, cfg *config.Config, p *proxy.Proxy, storage *storage.SQLiteStorage) error {
	ui := webui.New(cfg, p, storage)
	return ui.RegisterRoutes(mux)
}
