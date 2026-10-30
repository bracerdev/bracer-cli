package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The CLI side of pairing (spec backup/init-web § API contract): four
// calls on the hosted server, `Authorization: Pair <secret>` after the
// first. The secret never reaches a log line or --verbose.

// defaultServer is the hosted server `init --web` pairs with.
const defaultServer = "https://app.bracer.dev"

// pairAPI is what init --web needs from the server; tests fake it.
type pairAPI interface {
	// Open starts a session: POST /pair.
	Open(ctx context.Context, cliVersion, hostname string) (*pairOpened, error)
	// Next waits up to wait seconds for the web's next move: GET /pair/next.
	// nil, nil when nothing came.
	Next(ctx context.Context, secret string, wait int) (json.RawMessage, error)
	// State tells the web where the CLI is: POST /pair/state.
	State(ctx context.Context, secret string, body any) error
	// Finish ends the session; the token comes only with confirmed.
	Finish(ctx context.Context, secret string, confirmed bool, app string) (*pairFinished, error)
}

type pairOpened struct {
	Secret    string    `json:"pair_secret"`
	UserCode  string    `json:"user_code"`
	VerifyURL string    `json:"verify_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pairFinished struct {
	AppID string `json:"app_id"`
	Token string `json:"token"`
}

var (
	// errPairClosed: 410, the session is over (expired, finished, cancelled).
	errPairClosed = errors.New("pairing session closed")
	// errPairingOff: 503, the server has pairing switched off.
	errPairingOff = errors.New("pairing is switched off on the server")
	// errAppExists: 409 at finish, the account already has an app of that name.
	errAppExists = errors.New("app exists")
)

// pairError is any other answer the server gave.
type pairError struct {
	Status int
	Code   string
}

func (e *pairError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server answered %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("server answered %d", e.Status)
}

// httpPair is pairAPI over net/http.
type httpPair struct {
	server  string
	client  *http.Client
	verbose io.Writer // nil: quiet
}

func newHTTPPair(server string, verbose io.Writer) pairAPI {
	return &httpPair{server: server, client: &http.Client{Timeout: 45 * time.Second}, verbose: verbose}
}

func (h *httpPair) Open(ctx context.Context, cliVersion, hostname string) (*pairOpened, error) {
	var out pairOpened
	if err := h.call(ctx, http.MethodPost, "/pair", "", map[string]string{"cli_version": cliVersion, "hostname": hostname}, &out); err != nil {
		return nil, err
	}
	if out.Secret == "" || out.UserCode == "" || out.VerifyURL == "" {
		return nil, errors.New("server answered without a code")
	}
	return &out, nil
}

func (h *httpPair) Next(ctx context.Context, secret string, wait int) (json.RawMessage, error) {
	var raw json.RawMessage
	err := h.call(ctx, http.MethodGet, fmt.Sprintf("/pair/next?wait=%d", wait), secret, nil, &raw)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (h *httpPair) State(ctx context.Context, secret string, body any) error {
	return h.call(ctx, http.MethodPost, "/pair/state", secret, body, nil)
}

func (h *httpPair) Finish(ctx context.Context, secret string, confirmed bool, app string) (*pairFinished, error) {
	var out pairFinished
	body := map[string]any{"confirmed": confirmed, "app": app}
	if err := h.call(ctx, http.MethodPost, "/pair/finish", secret, body, &out); err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, nil
	}
	if out.Token == "" {
		return nil, errors.New("server answered without a token")
	}
	return &out, nil
}

// call does one request; a 204 leaves out untouched.
func (h *httpPair) call(ctx context.Context, method, path, secret string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.server+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Pair "+secret)
	}
	req.Header.Set("User-Agent", "bracer/"+Version)
	resp, err := h.client.Do(req)
	if err != nil {
		h.log("%s %s: %v", method, path, err)
		return &netError{err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	h.log("%s %s → %d", method, path, resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusGone:
		return errPairClosed
	case resp.StatusCode == http.StatusServiceUnavailable:
		return errPairingOff
	case resp.StatusCode >= 500:
		return &netError{&pairError{Status: resp.StatusCode}}
	case resp.StatusCode >= 300:
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		if resp.StatusCode == http.StatusConflict && e.Error == "APP_EXISTS" {
			return errAppExists
		}
		return &pairError{Status: resp.StatusCode, Code: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("server answered %d with something that is not JSON", resp.StatusCode)
	}
	return nil
}

// log writes one --verbose line; never a body, never the secret.
func (h *httpPair) log(format string, args ...any) {
	if h.verbose != nil {
		fmt.Fprintf(h.verbose, "pair: "+format+"\n", args...)
	}
}

// netError marks what is worth retrying: the network, or the server down.
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

func isNetError(err error) bool {
	var ne *netError
	return errors.As(err, &ne)
}

// maskedCode is the code as the customer reads it out: ABCD-EFGH.
func maskedCode(code string) string {
	if len(code) == 8 {
		return code[:4] + "-" + code[4:]
	}
	return strings.ToUpper(code)
}
