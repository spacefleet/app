// Package api contains the HTTP server implementation generated from
// api/openapi.yaml and the hand-written handlers that satisfy it.
//
// Regenerate with `make gen` after editing the spec.
package api

//go:generate go tool oapi-codegen --config=cfg.yaml ../../api/openapi.yaml
