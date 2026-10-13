package webhook

import "testing"

// Pins the documented openssl recipe in the README against the Go
// implementation, so the two cannot drift.
func TestSignMatchesOpenSSLReference(t *testing.T) {
	// printf '%s' '{"a":1}' | openssl dgst -sha256 -hmac 'local-test-secret-value'
	const want = "sha256=aa59333dede10d3bfb72c4fbf005c8142774d3201591d4cc47a0d960ef42f4f5"
	got := Sign([]byte(`{"a":1}`), []byte("local-test-secret-value"))
	if got != want {
		t.Fatalf("Sign() = %q, want the openssl reference %q", got, want)
	}
}
