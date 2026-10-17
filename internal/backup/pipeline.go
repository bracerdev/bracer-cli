package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// streamToStore runs a Postgres tool and streams its stdout straight to the bucket:
// tool → [age] → sha256 + byte count → Put (multipart). Never touches disk.
// SHA-256 and bytes are those of the stored object (after encryption). → ADR-0003, ADR-0008
func streamToStore(ctx context.Context, runner pg.Runner, conn pg.Conn, tool string, args []string, st storage.Store, key, recipient string) (bytes int64, sha string, err error) {
	pr, pw := io.Pipe()
	hasher := sha256.New()
	counter := &countWriter{}
	stored := io.MultiWriter(pw, hasher, counter)

	done := make(chan error, 1)
	go func() {
		// age writes its header as soon as the writer is created, so it must be created
		// in this goroutine: the read end of the pipe only opens once Put starts.
		var dumpDst io.Writer = stored
		var enc io.WriteCloser
		if recipient != "" {
			var err error
			enc, err = crypto.Encrypt(stored, recipient)
			if err != nil {
				pw.CloseWithError(err)
				done <- err
				return
			}
			dumpDst = enc
		}
		err := runner.Run(ctx, conn, tool, args, nil, dumpDst)
		if err == nil && enc != nil {
			err = enc.Close()
		}
		if err != nil {
			err = fmt.Errorf("%s: %w", tool, err)
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		done <- err
	}()

	putErr := st.Put(ctx, key, pr, -1)
	// Put failed midway: close the read end so the tool gets EPIPE and stops.
	pr.CloseWithError(putErr)
	dumpErr := <-done
	if dumpErr != nil {
		return 0, "", dumpErr
	}
	if putErr != nil {
		return 0, "", putErr
	}
	return counter.n, hex.EncodeToString(hasher.Sum(nil)), nil
}
