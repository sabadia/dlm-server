package cookies

import (
	"bytes"
	"errors"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := testCipher(t)
	aad := []byte("example.com\x00sid\x00/\x00")
	for _, plain := range []string{"", "v", "a longer cookie value; with=odd chars"} {
		sealed, err := c.Seal([]byte(plain), aad)
		if err != nil {
			t.Fatal(err)
		}
		if len(plain) > 4 && bytes.Contains(sealed, []byte(plain)) {
			t.Errorf("ciphertext contains the plaintext %q", plain)
		}
		got, err := c.Open(sealed, aad)
		if err != nil {
			t.Fatalf("Open(%q): %v", plain, err)
		}
		if string(got) != plain {
			t.Errorf("Open = %q, want %q", got, plain)
		}
	}
}

func TestCipherNonceIsRandom(t *testing.T) {
	c := testCipher(t)
	a, _ := c.Seal([]byte("same"), nil)
	b, _ := c.Seal([]byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same value are identical")
	}
}

func TestCipherRejects(t *testing.T) {
	c := testCipher(t)
	sealed, _ := c.Seal([]byte("secret"), []byte("aad"))
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	other, err := NewCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		c      *Cipher
		sealed []byte
		aad    []byte
	}{
		"tampered":  {c, tampered, []byte("aad")},
		"wrong aad": {c, sealed, []byte("other")},
		"wrong key": {other, sealed, []byte("aad")},
		"short":     {c, []byte{1, 2, 3}, nil},
		"empty":     {c, nil, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := tt.c.Open(tt.sealed, tt.aad); !errors.Is(err, ErrDecrypt) {
				t.Errorf("Open error = %v, want ErrDecrypt", err)
			}
		})
	}
}

func TestNewCipherKeySize(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("want an error for a 16-byte key")
	}
}
