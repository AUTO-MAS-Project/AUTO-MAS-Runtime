package health

import (
	"strings"
	"testing"
)

func TestSupervisionAuth_ProofBoundaries(t *testing.T) {
	secret, challenge := strings.Repeat("a", 64), strings.Repeat("b", 32)
	if got := CloseProof(secret, challenge, "1700000000", 36164); got != "74518ff85554a354cfe8c440e6a5b2d9e90c0de5618e049202a1711a8c8d0d55" {
		t.Fatalf("got %s, want Python golden vector", got)
	}
	body := []byte(`{"ready":true}`)
	signature := HealthProof(secret, challenge, body)
	for _, tt := range []struct {
		name, secret, challenge string
		body                    []byte
		signature               string
		want                    bool
	}{
		{name: "valid", secret: secret, challenge: challenge, body: body, signature: signature, want: true},
		{name: "wrong secret", secret: strings.Repeat("c", 64), challenge: challenge, body: body, signature: signature},
		{name: "replayed challenge", secret: secret, challenge: strings.Repeat("c", 32), body: body, signature: signature},
		{name: "tampered body", secret: secret, challenge: challenge, body: []byte(`{"ready":false}`), signature: signature},
		{name: "missing proof", secret: secret, challenge: challenge, body: body},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyHealthProof(tt.secret, tt.challenge, tt.body, tt.signature); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupervisionAuth_NewAttemptUsesFreshSecret(t *testing.T) {
	first, err := NewSupervisionSecret()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSupervisionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidSupervisionSecret(first) || !ValidSupervisionSecret(second) || first == second {
		t.Fatal("got invalid or reused secret")
	}
}
