// Package ent hosts the ent-generated persistence layer for Spacefleet.
// Schemas live in ./schema; regenerate the client with `make gen`.
package ent

//go:generate go tool ent generate --feature sql/versioned-migration ./schema
