package server

import (
	"net/http"
	"time"

	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/config"
)

func New(cfg *config.Config) *http.Server {
	auth.SetKey(cfg.ClerkSecretKey)

	mux := http.NewServeMux()
	registerRoutes(mux, cfg)

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
