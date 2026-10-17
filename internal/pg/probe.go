package pg

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// ProbeSSLRequest sends the Postgres protocol's SSLRequest packet and waits for
// one byte, S or N. No password is sent, the server logs nothing. true = is Postgres.
func ProbeSSLRequest(ctx context.Context, host string, port int) (bool, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	var msg [8]byte
	binary.BigEndian.PutUint32(msg[0:4], 8)
	binary.BigEndian.PutUint32(msg[4:8], 80877103)
	if _, err := conn.Write(msg[:]); err != nil {
		return false, err
	}
	var b [1]byte
	if _, err := conn.Read(b[:]); err != nil {
		return false, err
	}
	return b[0] == 'S' || b[0] == 'N', nil
}
