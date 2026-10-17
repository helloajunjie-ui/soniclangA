// Package keyvault implements the AES-256-GCM secret vault. By default a random
// machine key (data/vault/vault.key, 0600) is generated on first run and used to
// seal all secrets; a master-password (Argon2id) source is reserved for later
// (DESIGN §10.1).
package keyvault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Blob is the on-disk encrypted representation of a secret.
type Blob struct {
	Nonce  []byte `json:"nonce"`
	Cipher []byte `json:"cipher"`
	Salt   []byte `json:"salt,omitempty"` // used by master-password source
}

// Vault seals and unseals secrets under a machine key.
type Vault struct {
	dir string
	key []byte
	mu  sync.Mutex
}

const keyFile = "vault.key"

// New loads or generates the machine key under dataDir/vault.
//
// Both the vault directory and vault.key are locked down to the service account
// (hardenPath): the 0700/0600 modes below are advisory on Windows and the file
// would otherwise inherit whatever the parent directory grants, which is the
// difference between "the API keys are encrypted" and "the API keys are
// encrypted against other accounts on this machine" (DESIGN §10.1).
func New(dataDir string) (*Vault, error) {
	dir := filepath.Join(dataDir, "vault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := hardenPath(dir, true); err != nil {
		warnHardening(dir, err)
	}
	v := &Vault{dir: dir}
	kf := filepath.Join(dir, keyFile)
	if _, err := os.Stat(kf); os.IsNotExist(err) {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
		if err := os.WriteFile(kf, k, 0o600); err != nil {
			return nil, err
		}
		v.key = k
	} else {
		k, err := os.ReadFile(kf)
		if err != nil {
			return nil, err
		}
		v.key = k
	}
	// Re-harden on every boot, not only on creation: an existing vault.key from
	// an older build still carries the inherited ACL it was created with, and
	// that is the common case for anyone upgrading.
	if err := hardenPath(kf, false); err != nil {
		warnHardening(kf, err)
	}
	return v, nil
}

func (v *Vault) encrypt(plain []byte) (Blob, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return Blob{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Blob{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Blob{}, err
	}
	return Blob{Nonce: nonce, Cipher: gcm.Seal(nil, nonce, plain, nil)}, nil
}

func (v *Vault) decrypt(b Blob) ([]byte, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, b.Nonce, b.Cipher, nil)
}

// Set encrypts and persists a secret under id.
func (v *Vault) Set(id, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	blob, err := v.encrypt([]byte(secret))
	if err != nil {
		return err
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return err
	}
	kd := filepath.Join(v.dir, "keys")
	if err := os.MkdirAll(kd, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(kd, id+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(kd, id+".json"))
}

// Get decrypts and returns the secret stored under id.
func (v *Vault) Get(id string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(v.dir, "keys", id+".json"))
	if err != nil {
		return "", err
	}
	var b Blob
	if err := json.Unmarshal(data, &b); err != nil {
		return "", err
	}
	plain, err := v.decrypt(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// Delete removes a stored secret.
func (v *Vault) Delete(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return os.Remove(filepath.Join(v.dir, "keys", id+".json"))
}

// List returns stored secret ids (no plaintext).
func (v *Vault) List() ([]string, error) {
	kd := filepath.Join(v.dir, "keys")
	entries, err := os.ReadDir(kd)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			ids = append(ids, e.Name()[:len(e.Name())-5])
		}
	}
	return ids, nil
}
