// Package credential 负责解析 model_list 条目中的 API 凭据值。
//
// API Key 是一种授权凭据。本包集中处理原始凭据字符串（明文或文件引用）如何
// 解析为实际值，并让该逻辑与配置加载器保持分离。
//
// api_key 字段支持的格式：
//
//   - 明文:        "sk-abc123"           → 原样返回
//   - 文件引用:    "file://filename.key" → 读取 configDir/filename.key 的内容
//   - 加密:        "enc://<base64>"      → 通过 CHAGENT_KEY_PASSPHRASE 进行 AES-256-GCM 解密
//   - 空:          ""                    → 原样返回（auth_method=oauth 等）
//
// 加密采用 AES-256-GCM 与 HKDF-SHA256 密钥派生（< 1ms，适用于嵌入式 Linux）。
// 加密和解密都需要一个 SSH 私钥。
// 密钥派生：
//
//	HKDF-SHA256(ikm=HMAC-SHA256(SHA256(sshKeyBytes), passphrase), salt, info)
//
// SSH 密钥路径解析优先级：
//
//  1. Encrypt 的 sshKeyPath 参数（显式指定）
//  2. CHAGENT_SSH_KEY_PATH 环境变量
//  3. ~/.ssh/chagent_ed25519.key（os.UserHomeDir 跨平台）
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PassphraseEnvVar 是保存加密口令的环境变量。
// 其他包（如 config）引用该常量，以避免重复书写这个字符串。
const PassphraseEnvVar = "CHAGENT_KEY_PASSPHRASE"

// PassphraseProvider 是用于获取 enc:// 凭据解密口令的函数。
// 默认从进程环境中读取 CHAGENT_KEY_PASSPHRASE。可在启动时替换它，
// 改用其他来源（例如内存中的 SecureStore），这样各处的所有 LoadConfig()
// 调用都会共享同一口令来源，而无需借助 os.Environ。
//
// 示例（launcher main.go）：
//
//	credential.PassphraseProvider = apiHandler.passphraseStore.Get
var PassphraseProvider func() string = func() string {
	return os.Getenv(PassphraseEnvVar)
}

// ErrPassphraseRequired 表示遇到了 enc:// 凭据，但 PassphraseProvider
// 未提供可用口令。调用方可用 errors.Is 检测该错误，从而将“缺少口令”
// 与其他错误区分开。
var ErrPassphraseRequired = errors.New("credential: enc:// passphrase required")

// ErrDecryptionFailed 表示 enc:// 凭据无法解密，意味着口令或 SSH 密钥有误。
// 调用方可用 errors.Is 检测该错误。
var ErrDecryptionFailed = errors.New("credential: enc:// decryption failed (wrong passphrase or SSH key?)")

// SSHKeyPathEnvVar 是指定 enc:// 凭据加解密所用 SSH 私钥路径的环境变量。
const SSHKeyPathEnvVar = "CHAGENT_SSH_KEY_PATH"

// chagentHome 是 config.EnvHome 的包内副本。放在这里是为了
// 避免 pkg/credential 与 pkg/config 之间产生循环导入。
const chagentHome = "CHAGENT_HOME"

const (
	FileScheme = "file://"
	EncScheme  = "enc://"

	hkdfInfo = "chagent-credential-v1"
	saltLen  = 16
	nonceLen = 12
	keyLen   = 32
)

// Resolver 解析 model_list api_key 字段的原始凭据字符串。
// 文件引用相对于配置文件所在目录解析。
type Resolver struct {
	configDir         string
	resolvedConfigDir string // configDir 经符号链接解析后的形式
}

// NewResolver 返回一个 Resolver，它相对于 configDir（通常是配置文件路径的
// filepath.Dir）解析 file:// 引用。
func NewResolver(configDir string) *Resolver {
	resolved := configDir
	if configDir != "" {
		if linkedPath, err := filepath.EvalSymlinks(configDir); err == nil {
			resolved = linkedPath
		}
	}
	return &Resolver{configDir: configDir, resolvedConfigDir: resolved}
}

// Resolve 返回 raw 对应的实际凭据值：
//
//   - ""                → ""（无错误；auth_method=oauth 不需要密钥）
//   - "file://name.key" → configDir/name.key 去除首尾空白后的内容
//   - 其他任何值        → raw 原样返回（明文凭据）
func (r *Resolver) Resolve(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	if strings.HasPrefix(raw, FileScheme) {
		fileName := strings.TrimSpace(strings.TrimPrefix(raw, FileScheme))
		if fileName == "" {
			return "", fmt.Errorf("credential: file:// reference has no filename")
		}

		baseDir := r.resolvedConfigDir
		if baseDir == "" {
			baseDir = r.configDir
		}
		keyPath := filepath.Join(baseDir, fileName)
		// 在强制路径包含检查之前先解析符号链接，防止通过符号链接逃逸。
		realKeyPath, err := filepath.EvalSymlinks(keyPath)
		if err != nil {
			return "", fmt.Errorf("credential: failed to resolve credential file path %q: %w", keyPath, err)
		}
		if !isWithinDir(realKeyPath, baseDir) {
			return "", fmt.Errorf("credential: file:// path escapes config directory")
		}
		data, err := os.ReadFile(realKeyPath)
		if err != nil {
			return "", fmt.Errorf("credential: failed to read credential file %q: %w", realKeyPath, err)
		}

		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("credential: credential file %q is empty", realKeyPath)
		}

		return value, nil
	}

	if strings.HasPrefix(raw, EncScheme) {
		return resolveEncrypted(raw)
	}

	// 明文凭据 —— 原样返回。
	return raw, nil
}

// resolveEncrypted 使用 PassphraseProvider 解密 enc:// 凭据。
func resolveEncrypted(raw string) (string, error) {
	passphrase := PassphraseProvider()
	if passphrase == "" {
		return "", ErrPassphraseRequired
	}

	sshKeyPath := pickSSHKeyPath("") // override=""：先查询环境变量，再自动探测

	b64 := strings.TrimPrefix(raw, EncScheme)
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("credential: enc:// invalid base64: %w", err)
	}
	if len(blob) < saltLen+nonceLen+1 {
		return "", fmt.Errorf("credential: enc:// payload too short")
	}

	salt := blob[:saltLen]
	nonce := blob[saltLen : saltLen+nonceLen]
	ciphertext := blob[saltLen+nonceLen:]

	key, err := deriveKey(passphrase, sshKeyPath, salt)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("credential: enc:// cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("credential: enc:// gcm init: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDecryptionFailed, err)
	}
	return string(plaintext), nil
}

// Encrypt 加密 plaintext 并返回 enc:// 凭据字符串。
//
// passphrase 为必填项（即 CHAGENT_KEY_PASSPHRASE 的值）。
// sshKeyPath 为要使用的 SSH 私钥文件；传入 "" 则通过
// CHAGENT_SSH_KEY_PATH 环境变量或 ~/.ssh/chagent_ed25519.key 自动探测。
// 必须能够解析出 SSH 私钥，否则 Encrypt 返回错误。
func Encrypt(passphrase, sshKeyPath, plaintext string) (string, error) {
	if passphrase == "" {
		return "", fmt.Errorf("credential: passphrase must not be empty")
	}
	sshKeyPath = pickSSHKeyPath(sshKeyPath)

	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("credential: failed to generate salt: %w", err)
	}

	key, err := deriveKey(passphrase, sshKeyPath, salt)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("credential: cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("credential: gcm init: %w", err)
	}

	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("credential: failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	blob := make([]byte, 0, saltLen+nonceLen+len(ciphertext))
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return EncScheme + base64.StdEncoding.EncodeToString(blob), nil
}

// isWithinDir 报告 path 是否位于 dir 之内（或与 dir 相同）。
// 对相对路径使用 filepath.IsLocal，以稳健地跨平台检测目录穿越。
func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && filepath.IsLocal(rel)
}

// allowedSSHKeyPath 报告 path 是否位于 SSH 密钥文件允许的位置：
//   - 与 CHAGENT_SSH_KEY_PATH 环境变量完全一致
//   - 位于 CHAGENT_HOME 环境变量所指目录之内
//   - 位于 ~/.ssh/ 之内
func allowedSSHKeyPath(path string) bool {
	if path == "" {
		return true // 仅口令模式；不会读取任何文件
	}
	clean := filepath.Clean(path)

	// 与 CHAGENT_SSH_KEY_PATH 完全一致。
	if envPath, ok := os.LookupEnv(SSHKeyPathEnvVar); ok && envPath != "" {
		if clean == filepath.Clean(envPath) {
			return true
		}
	}

	// 位于 CHAGENT_HOME 之内。
	if picoHome := os.Getenv(chagentHome); picoHome != "" {
		if isWithinDir(clean, picoHome) {
			return true
		}
	}

	// 位于 ~/.ssh/ 之内。
	if userHome, err := os.UserHomeDir(); err == nil {
		if isWithinDir(clean, filepath.Join(userHome, ".ssh")) {
			return true
		}
	}

	return false
}

// deriveKey 从口令和 SSH 私钥派生出 32 字节的 AES-256 密钥。
//
// ikm = HMAC-SHA256(key=SHA256(sshKeyBytes), msg=passphrase)
// 最终密钥：HKDF-SHA256(ikm, salt, info="chagent-credential-v1", 32 字节)
// sshKeyPath 必须非空，否则返回错误。
func deriveKey(passphrase, sshKeyPath string, salt []byte) ([]byte, error) {
	if sshKeyPath == "" {
		return nil, fmt.Errorf(
			"credential: SSH private key is required but not found" +
				" (set CHAGENT_SSH_KEY_PATH or place key at ~/.ssh/chagent_ed25519.key)")
	}
	if !allowedSSHKeyPath(sshKeyPath) {
		return nil, fmt.Errorf(
			"credential: SSH key path %q is not in an allowed location (CHAGENT_SSH_KEY_PATH, CHAGENT_HOME, or ~/.ssh/)",
			sshKeyPath,
		)
	}
	sshBytes, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("credential: cannot read SSH key %q: %w", sshKeyPath, err)
	}
	sshHash := sha256.Sum256(sshBytes)
	mac := hmac.New(sha256.New, sshHash[:])
	mac.Write([]byte(passphrase))
	ikm := mac.Sum(nil)

	key, err := hkdf.Key(sha256.New, ikm, salt, hkdfInfo, keyLen)
	if err != nil {
		return nil, fmt.Errorf("credential: HKDF expand failed: %w", err)
	}
	return key, nil
}

// pickSSHKeyPath 返回用于加解密的 SSH 私钥路径。
//
// 优先级：
//  1. override（非空的显式参数）
//  2. CHAGENT_SSH_KEY_PATH 环境变量
//  3. ~/.ssh/chagent_ed25519.key（自动探测）
//
// 找不到密钥时返回 ""；此时 deriveKey 会返回错误。
func pickSSHKeyPath(override string) string {
	if override != "" {
		return override
	}
	if p, ok := os.LookupEnv(SSHKeyPathEnvVar); ok {
		return p // 尊重显式设置，即使其为 ""
	}
	return findDefaultSSHKey()
}

// findDefaultSSHKey 若存在 chagent 专用的 SSH 密钥路径，则返回该路径。
func findDefaultSSHKey() string {
	p, err := DefaultSSHKeyPath()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}
