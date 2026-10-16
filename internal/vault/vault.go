// Package vault 将 AES-GCM 主密钥保存在系统凭证库；密文留在 SQLite。
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"

	"github.com/zalando/go-keyring"
)

type Vault struct {
	mu      sync.Mutex
	account string
	aead    cipher.AEAD
}

const service = "io.im-agent.desktop"

func New(account string) *Vault { return &Vault{account: account} }
func (v *Vault) Unlock(hasCiphertext bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.aead != nil {
		return nil
	}
	encoded, err := keyring.Get(service, v.account)
	if errors.Is(err, keyring.ErrNotFound) {
		if hasCiphertext {
			return errors.New("系统凭证库中缺少原主密钥，无法解密已有数据；请恢复原钥匙串，勿重建密钥")
		}
		key := make([]byte, 32)
		if _, err = io.ReadFull(rand.Reader, key); err != nil {
			return errors.New("无法生成加密主密钥")
		}
		encoded = base64.StdEncoding.EncodeToString(key)
		if err = keyring.Set(service, v.account, encoded); err != nil {
			return errors.New("无法保存系统凭证库主密钥；请允许钥匙串访问，不会降级明文")
		}
	} else if err != nil {
		return errors.New("系统凭证库不可访问，请解锁或允许访问后重试")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return errors.New("系统凭证库主密钥格式无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return errors.New("无法初始化加密")
	}
	v.aead, err = cipher.NewGCM(block)
	return err
}
func (v *Vault) Seal(scope, secret string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.aead == nil {
		return nil, errors.New("凭证库未解锁")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, e := io.ReadFull(rand.Reader, nonce); e != nil {
		return nil, errors.New("生成随机数失败")
	}
	return v.aead.Seal(nonce, nonce, []byte(secret), []byte(scope)), nil
}
func (v *Vault) Open(scope string, data []byte) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.aead == nil {
		return "", errors.New("凭证库未解锁")
	}
	n := v.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("密文不完整")
	}
	value, e := v.aead.Open(nil, data[:n], data[n:], []byte(scope))
	if e != nil {
		return "", errors.New("解密失败：密文或主密钥不匹配")
	}
	return string(value), nil
}
