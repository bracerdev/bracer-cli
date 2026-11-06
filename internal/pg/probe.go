package pg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// ProbeGSSENCRequest sends the Postgres protocol's GSSENCRequest packet and
// waits for one byte, G or N, and nothing after it. No password is sent.
// true = is Postgres. Not SSLRequest: Traefik answers that one with S on every
// entrypoint (it speaks Postgres STARTTLS), so its HTTP and HTTPS ports passed.
// One deadline of probeBudget covers dial, write, the answer and the silence
// check after it.
func ProbeGSSENCRequest(ctx context.Context, host string, port int) (bool, error) {
	deadline := time.Now().Add(probeBudget)
	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	var msg [8]byte
	binary.BigEndian.PutUint32(msg[0:4], 8)
	binary.BigEndian.PutUint32(msg[4:8], 80877104)
	if _, err := conn.Write(msg[:]); err != nil {
		return false, err
	}
	var b [1]byte
	if _, err := conn.Read(b[:]); err != nil {
		return false, err
	}
	if b[0] != 'G' && b[0] != 'N' {
		return false, nil
	}
	// Postgres sends exactly that byte and waits, connection open, for the
	// startup packet; anything that keeps talking or hangs up is something else.
	if quiet := time.Now().Add(300 * time.Millisecond); quiet.Before(deadline) {
		_ = conn.SetReadDeadline(quiet)
	}
	n, err := conn.Read(b[:])
	var ne net.Error
	return n == 0 && errors.As(err, &ne) && ne.Timeout(), nil
}

// probeBudget is the most one port may take (spec init § Flow 4b).
const probeBudget = 2 * time.Second
