package health

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

const (
	// SecretEnvironment 是只经子进程环境注入的受监督密钥键。
	SecretEnvironment = "AUTO_MAS_SUPERVISION_SECRET"
	// ChallengeHeader、ProofHeader 和 TimestampHeader 定义 C21 的 HTTP 鉴权头。
	ChallengeHeader = "X-Auto-MAS-Challenge"
	ProofHeader     = "X-Auto-MAS-Proof"
	TimestampHeader = "X-Auto-MAS-Timestamp"
)

// NewSupervisionSecret 为一次启动生成独立密钥。
func NewSupervisionSecret() (string, error) { return randomHex(32) }

// NewChallenge 为单次请求生成随机挑战，防止复用健康证明。
func NewChallenge() (string, error) { return randomHex(16) }

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// ValidSupervisionSecret 校验密钥的受控编码，不把密钥带入错误信息。
func ValidSupervisionSecret(secret string) bool { return validHex(secret, 64) }

func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HealthProof 绑定本次挑战与原始健康正文，避免 JSON 序列化差异。
func HealthProof(secret, challenge string, body []byte) string {
	return proof(secret, append([]byte("auto-mas-health-v1\n"+challenge+"\n"), body...))
}

// VerifyHealthProof 以常量时间比较证明，并拒绝非契约编码。
func VerifyHealthProof(secret, challenge string, body []byte, received string) bool {
	return ValidSupervisionSecret(secret) && validHex(challenge, 32) && validHex(received, 64) &&
		hmac.Equal([]byte(HealthProof(secret, challenge, body)), []byte(received))
}

// CloseProof 使用独立域绑定关闭端点、端口、时间与挑战，不传输密钥。
func CloseProof(secret, challenge, timestamp string, port int) string {
	return proof(secret, []byte("auto-mas-close-v1\nPOST\n/api/core/close\n"+strconv.Itoa(port)+"\n"+timestamp+"\n"+challenge))
}

func proof(secret string, message []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(message) // hash.Hash.Write 保证不返回错误。
	return hex.EncodeToString(mac.Sum(nil))
}
