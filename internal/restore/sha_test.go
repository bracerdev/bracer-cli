package restore

import "crypto/sha256"

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
