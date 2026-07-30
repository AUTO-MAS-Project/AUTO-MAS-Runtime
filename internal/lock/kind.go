package lock

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Kind 标识一把 Runtime 命名 Mutex 的用途。
type Kind string

const (
	// KindBackend 保证同一 app root 只有一个后端监督进程。
	KindBackend Kind = "backend"
	// KindMutation 串行化同一 app root 的受管变更。
	KindMutation Kind = "mutation"

	mutexDomain = "AUTO-MAS-Runtime/app-root/v1\x00"
)

// String 返回 Kind 的稳定文本。
func (k Kind) String() string {
	return string(k)
}

// Valid 报告 Kind 是否属于冻结全集。
func (k Kind) Valid() bool {
	return k == KindBackend || k == KindMutation
}

type rootIdentity struct {
	volumeSerial uint64
	fileID       [16]byte
}

func mutexName(kind Kind, identity rootIdentity) (string, error) {
	if !kind.Valid() {
		return "", fmt.Errorf("invalid mutex kind %q", kind)
	}
	payload := make([]byte, 0, len(mutexDomain)+8+len(identity.fileID))
	payload = append(payload, mutexDomain...)
	var serial [8]byte
	binary.LittleEndian.PutUint64(serial[:], identity.volumeSerial)
	payload = append(payload, serial[:]...)
	payload = append(payload, identity.fileID[:]...)
	digest := sha256.Sum256(payload)
	return "Local\\AUTO-MAS-Runtime-" + kind.String() + "-" +
		hex.EncodeToString(digest[:16]), nil
}
