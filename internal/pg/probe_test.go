package pg

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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

// BR-029: a TCP port is Postgres only when GSSENCRequest gets exactly one G or
// N back. A reverse proxy's HTTP and HTTPS ports do not answer that way, even
// one that answers SSLRequest with S as Postgres STARTTLS (Traefik does).
func TestBR029ProbeGSSENCRequest(t *testing.T) {
	answer := func(b byte) func(net.Conn) {
		return func(c net.Conn) {
			var req [8]byte
			io.ReadFull(c, req[:])
			if binary.BigEndian.Uint32(req[4:]) == 80877104 {
				c.Write([]byte{b})
			}
			time.Sleep(time.Second)
		}
	}
	sslRequest := []byte{0, 0, 0, 8, 4, 210, 22, 47}
	// A proxy that speaks Postgres STARTTLS: S to SSLRequest, anything else
	// goes to its HTTP server, which waits for a request line.
	starttlsProxy := serve(t, func(c net.Conn) {
		var req [8]byte
		io.ReadFull(c, req[:])
		if bytes.Equal(req[:], sslRequest) {
			c.Write([]byte("S"))
		}
		time.Sleep(3 * time.Second)
	})
	httpSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(httpSrv.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	cases := []struct {
		name string
		port int
		want bool
	}{
		{"postgres without GSSAPI", serve(t, answer('N')), true},
		{"postgres built with GSSAPI", serve(t, answer('G')), true},
		{"sshd banner", serve(t, func(c net.Conn) {
			c.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
			time.Sleep(time.Second)
		}), false},
		{"http on 80", httpSrv.Listener.Addr().(*net.TCPAddr).Port, false},
		{"tls on 443", tlsSrv.Listener.Addr().(*net.TCPAddr).Port, false},
		{"http 400", serve(t, func(c net.Conn) {
			io.ReadFull(c, make([]byte, 8))
			c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		}), false},
		{"tls alert", serve(t, func(c net.Conn) {
			io.ReadFull(c, make([]byte, 8))
			c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x32})
		}), false},
		{"postgres STARTTLS proxy", starttlsProxy, false},
		{"closes at once", serve(t, func(net.Conn) {}), false},
	}
	var wg sync.WaitGroup
	for _, tc := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := ProbeGSSENCRequest(context.Background(), "127.0.0.1", tc.port); ok != tc.want {
				t.Errorf("%s: got %v, want %v", tc.name, ok, tc.want)
			}
		}()
	}
	wg.Wait()
}
