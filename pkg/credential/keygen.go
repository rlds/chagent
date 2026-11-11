package credential

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// DefaultSSHKeyPath 返回 chagent 专用 SSH 密钥的规范路径。
// 该路径始终为 ~/.ssh/chagent_ed25519.key（os.UserHomeDir 跨平台）。
func DefaultSSHKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credential: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".ssh", "chagent_ed25519.key"), nil
}

// GenerateSSHKey 生成 Ed25519 SSH 密钥对，并将私钥写入 path（权限 0600）、
// 公钥写入 path+".pub"（权限 0644）。
// 若 ~/.ssh/ 目录不存在，则以 0700 权限创建。
// 若文件已存在，则会被覆盖。
func GenerateSSHKey(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("credential: keygen: cannot create directory %q: %w", filepath.Dir(path), err)
	}

	pubRaw, privRaw, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("credential: keygen: ed25519 key generation failed: %w", err)
	}

	// 将私钥序列化为 OpenSSH PEM。
	block, err := ssh.MarshalPrivateKey(privRaw, "")
	if err != nil {
		return fmt.Errorf("credential: keygen: marshal private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(block)

	if err = os.WriteFile(path, privPEM, 0o600); err != nil {
		return fmt.Errorf("credential: keygen: write private key %q: %w", path, err)
	}

	// 将公钥序列化为 authorized_keys 行。
	sshPub, err := ssh.NewPublicKey(pubRaw)
	if err != nil {
		return fmt.Errorf("credential: keygen: marshal public key: %w", err)
	}
	pubLine := ssh.MarshalAuthorizedKey(sshPub)

	pubPath := path + ".pub"
	if err := os.WriteFile(pubPath, pubLine, 0o644); err != nil {
		return fmt.Errorf("credential: keygen: write public key %q: %w", pubPath, err)
	}

	return nil
}
