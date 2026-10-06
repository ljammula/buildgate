package main

import (
	"bufio"
	"net"
	"reflect"
	"strconv"
	"testing"
)

func TestParseForwards(t *testing.T) {
	got, err := ParseForwards("5433=postgres:5432,6380=redis:6379")
	if err != nil {
		t.Fatal(err)
	}
	want := []Forward{{Port: 5433, Target: "postgres:5432"}, {Port: 6380, Target: "redis:6379"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseForwards = %+v, want %+v", got, want)
	}
	if got, err := ParseForwards(""); err != nil || got != nil {
		t.Fatalf(`ParseForwards("") = %+v, %v; want none`, got, err)
	}
	for _, bad := range []string{"5433", "x=postgres:5432", "0=postgres:5432", "5433=postgres", "5433=:5432", "5433=postgres:"} {
		if _, err := ParseForwards(bad); err == nil {
			t.Errorf("ParseForwards(%q) accepted a malformed entry", bad)
		}
	}
}

// TestListenForwardsLocalhostOnBothLoopbacks: a client dialing
// localhost:<port> over either loopback reaches the target, and bytes flow
// both ways.
func TestListenForwardsLocalhostOnBothLoopbacks(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				_, _ = conn.Write([]byte("echo:" + line))
			}()
		}
	}()

	port := freePort(t)
	listeners, err := Listen([]Forward{{Port: port, Target: upstream.Addr().String()}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	for _, l := range listeners {
		defer l.Close()
		go l.serve()
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			if host == "::1" {
				t.Logf("no IPv6 loopback here: %v", err)
				continue
			}
			t.Fatalf("dial %s: %v", host, err)
		}
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			t.Fatal(err)
		}
		reply, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil || reply != "echo:ping\n" {
			t.Fatalf("reply over %s = %q, %v; want echo:ping", host, reply, err)
		}
	}
}

// TestListenFailsWhenThePortIsTaken: a port already bound in the worker is a
// startup error, never a silently missing forward.
func TestListenFailsWhenThePortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	if _, err := Listen([]Forward{{Port: port, Target: "postgres:5432"}}); err == nil {
		t.Fatal("Listen succeeded on a port already in use")
	}
}

func TestRunPropagatesTheCommandsExitStatus(t *testing.T) {
	if got := run([]string{"--", "sh", "-c", "exit 3"}, ""); got != 3 {
		t.Fatalf("exit status = %d, want 3", got)
	}
	if got := run([]string{"--", "sh", "-c", "kill -TERM $$"}, ""); got != 128+15 {
		t.Fatalf("exit status after SIGTERM = %d, want 143", got)
	}
	if got := run([]string{"sh"}, ""); got != 2 {
		t.Fatalf("exit status without -- = %d, want 2", got)
	}
	if got := run([]string{"--", "true"}, "garbage"); got != 2 {
		t.Fatalf("exit status on a malformed BG_COMPOSE_FORWARDS = %d, want 2", got)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
