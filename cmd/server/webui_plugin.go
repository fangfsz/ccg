package main

import (
	"net/http"

	"github.com/fangfsz/ccg/internal/config"
	"github.com/fangfsz/ccg/internal/proxy"
	"github.com/fangfsz/ccg/internal/storage"
	"github.com/fangfsz/ccg/cmd/server/webui"
)

// registerWebUI registers the Web UI routes
func registerWebUI(mux *http.ServeMux, cfg *config.Config, p *proxy.Proxy, storage *storage.SQLiteStorage) error {
	ui := webui.New(cfg, p, storage)
	return ui.RegisterRoutes(mux)
}
