// Package webhook verifies, parses and routes GitHub webhook deliveries.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader is the only signature header Shuriken accepts. The older
// X-Hub-Signature (SHA-1) header is deliberately ignored: accepting it would
// let a caller downgrade us to a broken MAC.
const SignatureHeader = "X-Hub-Signature-256"

var (
	// ErrMissingSignature means the header was absent or empty.
	ErrMissingSignature = errors.New("webhook: missing X-Hub-Signature-256 header")
	// ErrMalformedSignature means the header was not "sha256=<64 hex chars>".
	ErrMalformedSignature = errors.New("webhook: malformed signature header")
	// ErrSignatureMismatch means the MAC did not match the body.
	ErrSignatureMismatch = errors.New("webhook: signature mismatch")
)

// VerifySignature checks an X-Hub-Signature-256 header against the raw request
// body using the shared webhook secret. The comparison is constant time.
//
// The body must be the exact bytes GitHub sent. Re-serialising the JSON before
// verifying will produce a mismatch.
func VerifySignature(header string, body, secret []byte) error {
	if strings.TrimSpace(header) == "" {
		return ErrMissingSignature
	}

	prefix, hexMAC, found := strings.Cut(header, "=")
	if !found || prefix != "sha256" {
		return ErrMalformedSignature
	}

	want, err := hex.DecodeString(hexMAC)
	if err != nil || len(want) != sha256.Size {
		return ErrMalformedSignature
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), want) {
		return ErrSignatureMismatch
	}
	return nil
}

// Sign produces the header value GitHub would send for a body. It exists for
// tests and for the local delivery replay documented in the README.
func Sign(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
