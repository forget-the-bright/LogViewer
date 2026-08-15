// Package cryptoutil 提供配置文件密码字段的 AES-256-GCM 加解密。
//
// 设计原则：
//   - 密钥由用户提供的 passphrase 经 SHA-256 派生为 32 字节 AES 密钥；
//   - 加密结果格式为 enc:v1:<base64(nonce||ciphertext)>，可在 JSON 中直接存储；
//   - 非 enc:v1: 前缀的字符串视为明文原样返回，兼容未加密配置。
package cryptoutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// Prefix 是加密字符串的标识前缀。
	Prefix = "enc:v1:"
)

// DeriveKey 将任意长度的 passphrase 派生为 32 字节 AES-256 密钥。
func DeriveKey(passphrase string) []byte {
	h := sha256.Sum256([]byte(passphrase))
	return h[:]
}

// IsEncrypted 判断字符串是否为加密格式。
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// Encrypt 使用 AES-256-GCM 加密明文。
// passphrase 会被 SHA-256 派生为 32 字节密钥。
// 返回格式：enc:v1:<base64(nonce||ciphertext)>。
func Encrypt(passphrase, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	key := DeriveKey(passphrase)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建 cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	// ciphertext = nonce || sealed
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 enc:v1:... 格式的字符串。
// 如果输入不是加密格式（没有 enc:v1: 前缀），原样返回明文（兼容未加密配置）。
func Decrypt(passphrase, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if !IsEncrypted(encoded) {
		return encoded, nil // 明文，原样返回
	}
	payload := strings.TrimPrefix(encoded, Prefix)
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("解码加密数据失败: %w", err)
	}
	key := DeriveKey(passphrase)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建 cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("加密数据格式损坏：长度不足")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥是否正确？）: %w", err)
	}
	return string(plaintext), nil
}
