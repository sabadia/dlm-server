package hls

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// ivFor returns the IV of a segment: the key's explicit IV, else the media
// sequence number as a 128-bit big-endian integer (RFC 8216 §5.2).
func ivFor(k *keyInfo, seq uint64) []byte {
	if len(k.iv) == aes.BlockSize {
		return k.iv
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], seq)
	return iv
}

// decryptAES128 decrypts a whole AES-128-CBC segment and removes its PKCS#7
// padding. key must be 16 bytes.
func decryptAES128(data, key, iv []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("hls: the AES-128 key is %d bytes, want 16", len(key))
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("hls: encrypted segment of %d bytes is not a multiple of the block size", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("hls: %w", err)
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("hls: bad padding after decrypting a segment (wrong key?)")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, errors.New("hls: bad padding after decrypting a segment (wrong key?)")
		}
	}
	return out[:len(out)-pad], nil
}
