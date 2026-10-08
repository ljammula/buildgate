package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A console tab's event stream or an MCP client's session stream is a GET that
// stays open for as long as the client likes. Shutdown must end it rather than
// wait its whole deadline for it, and must still let a write in flight finish.
func TestServeShutdownEndsOpenReadsAndLetsWritesFinish(t *testing.T) {
	shutdown, signal := context.WithCancel(context.Background())
	defer signal()
	writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
	writeSawCancel := make(chan bool, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		close(writeEntered)
		<-releaseWrite
		writeSawCancel <- r.Context().Err() != nil
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(endReadsOnShutdown(mux, shutdown))
	defer srv.Close()

	stream, err := http.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamEnded := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stream.Body); close(streamEnded) }()
	writeStatus := make(chan int, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/write", "text/plain", nil)
		if err != nil {
			writeStatus <- 0
			return
		}
		resp.Body.Close()
		writeStatus <- resp.StatusCode
	}()
	<-writeEntered

	select {
	case <-streamEnded:
		t.Fatal("the stream ended before shutdown was signalled")
	case <-time.After(100 * time.Millisecond):
	}
	signal()
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		shutdownDone <- srv.Config.Shutdown(ctx)
	}()
	select {
	case <-streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the open stream was still open 5 s after shutdown was signalled")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned (%v) while a write was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseWrite)
	if <-writeSawCancel {
		t.Error("the write's context was cancelled by shutdown; only reads are ended")
	}
	if got := <-writeStatus; got != http.StatusNoContent {
		t.Errorf("the write in flight answered %d, want %d", got, http.StatusNoContent)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish once the write had")
	}
}
