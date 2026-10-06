package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newTodoID returns a random todo identifier. It's generated once, by the
// API, and shared by both the in-memory Store (for the immediate POST
// response) and the Kafka event cmd/consumer later uses to write the same
// row into Postgres -- so the id a client sees is the same id that
// eventually lands durably.
func newTodoID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
