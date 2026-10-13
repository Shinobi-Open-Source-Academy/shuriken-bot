package webhook

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret := []byte("it's a secret to everybody")
	body := []byte(`{"action":"opened"}`)
	valid := Sign(body, secret)

	tests := []struct {
		name   string
		header string
		body   []byte
		secret []byte
		want   error
	}{
		{"valid signature", valid, body, secret, nil},
		{"empty header", "", body, secret, ErrMissingSignature},
		{"whitespace header", "   ", body, secret, ErrMissingSignature},
		{"no prefix", strings.TrimPrefix(valid, "sha256="), body, secret, ErrMalformedSignature},
		{
			// A sha1 header must be refused outright, not silently accepted,
			// or a caller could downgrade us to a broken MAC.
			name:   "sha1 prefix rejected",
			header: "sha1=aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d",
			body:   body, secret: secret, want: ErrMalformedSignature,
		},
		{"not hex", "sha256=zzzz", body, secret, ErrMalformedSignature},
		{"wrong digest length", "sha256=abcd", body, secret, ErrMalformedSignature},
		{"wrong secret", Sign(body, []byte("other secret value")), body, secret, ErrSignatureMismatch},
		{"tampered body", valid, []byte(`{"action":"closed"}`), secret, ErrSignatureMismatch},
		{"empty body with matching signature", Sign(nil, secret), nil, secret, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySignature(tc.header, tc.body, tc.secret)
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifySignature() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSignIsStable(t *testing.T) {
	secret := []byte("stable-secret-value")
	body := []byte("payload")
	if a, b := Sign(body, secret), Sign(body, secret); a != b {
		t.Fatalf("Sign is not deterministic: %q != %q", a, b)
	}
}

// Case in the hex digest should not matter; GitHub sends lowercase but the
// decoder accepts either.
func TestVerifySignatureUppercaseHex(t *testing.T) {
	secret := []byte("another-secret-value")
	body := []byte("payload")
	upper := "sha256=" + strings.ToUpper(strings.TrimPrefix(Sign(body, secret), "sha256="))
	if err := VerifySignature(upper, body, secret); err != nil {
		t.Fatalf("uppercase hex rejected: %v", err)
	}
}
