package identity

import "testing"

func TestHashTokenIsDeterministic(t *testing.T) {
	a := hashToken("secret")
	b := hashToken("secret")
	if a != b {
		t.Fatalf("hashToken not deterministic: %q vs %q", a, b)
	}
}

func TestHashTokenDiffersForDifferentInput(t *testing.T) {
	if hashToken("a") == hashToken("b") {
		t.Fatal("hashToken collision for different inputs")
	}
}

func TestHashTokenNotRaw(t *testing.T) {
	raw := "my-token"
	if hashToken(raw) == raw {
		t.Fatal("hashToken returned raw token")
	}
}

func TestGenerateTokenUnique(t *testing.T) {
	t1, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	t2, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if t1 == t2 {
		t.Fatal("generateToken produced duplicate tokens")
	}
	if len(t1) != 64 { // 32 bytes hex-encoded
		t.Fatalf("unexpected token length %d", len(t1))
	}
}
