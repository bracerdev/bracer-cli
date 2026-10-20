package pg

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// serve accepts one connection and runs handle on it.
func serve(t *testing.T, handle func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		handle(c)
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// Postgres answers SSLRequest with one byte and waits; sshd's banner also
// starts with S but more follows; HTTP answers with something else.
func TestProbeSSLRequest(t *testing.T) {
	postgres := serve(t, func(c net.Conn) {
		io.ReadFull(c, make([]byte, 8))
		c.Write([]byte("N"))
		time.Sleep(time.Second)
	})
	sshd := serve(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
		time.Sleep(time.Second)
	})
	http := serve(t, func(c net.Conn) {
		io.ReadFull(c, make([]byte, 8))
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
	})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		port int
		want bool
	}{{"postgres", postgres, true}, {"sshd", sshd, false}, {"http", http, false}} {
		if ok, _ := ProbeSSLRequest(ctx, "127.0.0.1", tc.port); ok != tc.want {
			t.Errorf("%s: got %v", tc.name, ok)
		}
	}
}
