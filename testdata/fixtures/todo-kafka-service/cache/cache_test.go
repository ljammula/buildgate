package cache

import (
	"net"
	"testing"
	"time"

	"todo-service/pgstore"
)

type fakeLister []pgstore.Todo

func (f fakeLister) List() ([]pgstore.Todo, error) { return f, nil }

// TestListCacheUnresponsiveRedisFallsThroughFast: a Redis that accepts
// connections but never answers (a blackholed host) must cost a read at
// most one redisCallTimeout, not the client's multi-second retries, and
// the read is still served from next.
func TestListCacheUnresponsiveRedisFallsThroughFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	rdb := newClient(ln.Addr().String())
	t.Cleanup(func() { rdb.Close() })
	want := fakeLister{{ID: "1", Title: "from postgres"}}
	c := NewListCache(rdb, want, time.Minute)

	start := time.Now()
	got, err := c.List()
	elapsed := time.Since(start)

	if err != nil || len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("List = %+v, %v; want the lister's result", got, err)
	}
	if elapsed > 5*redisCallTimeout {
		t.Fatalf("List took %v with an unresponsive Redis, want about %v", elapsed, redisCallTimeout)
	}
}
