package segmented

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestIVFor(t *testing.T) {
	iv := ivFor(&Key{}, 258)
	if len(iv) != 16 || iv[14] != 1 || iv[15] != 2 || iv[0] != 0 {
		t.Errorf("sequence IV = %x", iv)
	}
	explicit := &Key{IV: bytes.Repeat([]byte{7}, 16)}
	if !bytes.Equal(ivFor(explicit, 5), explicit.IV) {
		t.Error("an explicit IV was not used")
	}
}

func encrypt(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

func TestDecryptAES128(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 16)
	iv := ivFor(&Key{}, 9)
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		plain := bytes.Repeat([]byte{0xAB}, n)
		got, err := decryptAES128(encrypt(t, plain, key, iv), key, iv)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("round trip of %d bytes: %v", n, err)
		}
	}
	enc := encrypt(t, []byte("secret segment"), key, iv)
	if _, err := decryptAES128(enc, bytes.Repeat([]byte{4}, 16), iv); err == nil {
		t.Error("a wrong key was accepted")
	}
	for name, tc := range map[string]struct{ data, key []byte }{
		"short key": {enc, key[:8]}, "empty": {nil, key}, "not a block multiple": {append(bytes.Clone(enc), 0), key},
	} {
		if _, err := decryptAES128(tc.data, tc.key, iv); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestContainerSelection(t *testing.T) {
	for name, want := range map[string]string{"a.mp4": "mp4", "a.MKV": "matroska", "a.ts": "mpegts", "a.m4a": "ipod", "a.bin": "mp4"} {
		if got, _ := containerFor(name); got != want {
			t.Errorf("containerFor(%q) = %q, want %q", name, got, want)
		}
	}
	if _, fast := containerFor("a.mkv"); fast {
		t.Error("mkv needs no faststart")
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("https://user:pw@cdn.example/a.ts?token=SECRET&x=1"); bytes.Contains([]byte(got), []byte("SECRET")) || bytes.Contains([]byte(got), []byte("pw")) {
		t.Errorf("Redact leaks: %q", got)
	}
}

var (
	_ cipher.Block
	_ = aes.BlockSize
)
