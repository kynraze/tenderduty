package tenderduty

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

var plainText []byte

func TestEncrypt(t *testing.T) {

	randLen := time.Now().Unix() % 1000 // not really random, just don't want every run to check same size file
	plainText = make([]byte, 997+randLen)
	_, err := rand.Read(plainText)
	if err != nil {
		t.Fatal(err)
	}

	_, err = encrypt(plainText, "password1")
	if err == nil {
		t.Error("well-known passwords should be rejected")
	}
	err = nil
	_, err = encrypt(plainText, "4G*vk90")
	if err == nil {
		t.Error("short passwords should be rejected")
	}
	err = nil

	// try to get some variability in passwords for encrypt/decrypt testing
	passBytes := make([]byte, int(time.Now().Second()%10+9))
	_, err = rand.Read(passBytes)
	if err != nil {
		t.Fatal(err)
	}
	password := make([]byte, base64.StdEncoding.EncodedLen(len(passBytes)))
	base64.StdEncoding.Encode(password, passBytes)

	cipherText, err := encrypt(plainText, string(password[:len(passBytes)]))
	if err != nil {
		t.Error(err)
		return
	}
	if len(cipherText) == 0 {
		t.Error("encryption failed, got 0 length ciphertext")
		return
	}

	plainText2, err := decrypt(cipherText, string(password[:len(passBytes)]))
	if err != nil {
		t.Error("decryption failed", err)
		return
	}
	if len(plainText2) == 0 {
		t.Error("decryption failed, got empty plaintext")
	}
	if !bytes.Equal(plainText, plainText2) {
		t.Error("plaintext does not match after decrypting")
	}

}

func TestDecryptAlignedPlaintextAndMalformedFiles(t *testing.T) {
	for _, size := range []int{1, 15, 16, 17, 32} {
		plain := bytes.Repeat([]byte("x"), size)
		encoded, err := encrypt(plain, "Strong-test-passphrase-123!")
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decrypt(encoded, "Strong-test-passphrase-123!")
		if err != nil || !bytes.Equal(decoded, plain) {
			t.Fatalf("size %d: incorrect decryption: %v", size, err)
		}
	}
	for _, encoded := range [][]byte{nil, []byte("AA=="), []byte("invalid"), []byte(base64.StdEncoding.EncodeToString(make([]byte, 81)))} {
		if _, err := decrypt(encoded, "Strong-test-passphrase-123!"); err == nil {
			t.Fatal("malformed encrypted file was accepted")
		}
	}
}

func TestDecryptPreservesTrailingZeroMACByte(t *testing.T) {
	password := "Strong-test-passphrase-123!"
	key, macKey, salt, err := getKey(password, bytes.Repeat([]byte{1}, idKeySize))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte{'x'}, aes.BlockSize)
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{aes.BlockSize}, aes.BlockSize)...)
	for counter := uint32(0); counter < 4096; counter++ {
		iv := make([]byte, ivSize)
		binary.LittleEndian.PutUint32(iv, counter)
		encrypted := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)
		payload := append(append(append([]byte(nil), salt...), iv...), encrypted...)
		mac := hmac.New(sha256.New, macKey)
		_, _ = mac.Write(payload)
		sum := mac.Sum(nil)
		if sum[len(sum)-1] != 0 {
			continue
		}
		encoded := []byte(base64.StdEncoding.EncodeToString(append(payload, sum...)))
		decoded, err := decrypt(encoded, password)
		if err != nil || !bytes.Equal(decoded, plain) {
			t.Fatalf("valid trailing zero MAC failed: %v", err)
		}
		return
	}
	t.Fatal("could not construct the trailing-zero fixture")
}
