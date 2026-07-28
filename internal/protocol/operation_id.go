package protocol

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"time"
)

const (
	operationIDLength = 26
	ulidAlphabet      = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// newOperationID returns a canonical ULID containing the supplied timestamp.
func newOperationID(now time.Time, random io.Reader) (string, error) {
	var data [16]byte

	milliseconds := now.UnixMilli()
	if milliseconds < 0 || milliseconds > 1<<48-1 {
		return "", fmt.Errorf("ULID timestamp is outside the 48-bit range")
	}
	for i := 5; i >= 0; i-- {
		data[i] = byte(milliseconds)
		milliseconds >>= 8
	}
	if _, err := io.ReadFull(random, data[6:]); err != nil {
		return "", fmt.Errorf("generate ULID entropy: %w", err)
	}

	value := new(big.Int).SetBytes(data[:])
	base := big.NewInt(32)
	remainder := new(big.Int)
	encoded := [operationIDLength]byte{}
	for i := operationIDLength - 1; i >= 0; i-- {
		value.QuoRem(value, base, remainder)
		encoded[i] = ulidAlphabet[remainder.Int64()]
	}
	return string(encoded[:]), nil
}

func validOperationID(value string) bool {
	if len(value) != operationIDLength || value[0] > '7' {
		return false
	}
	for i := range len(value) {
		if !isULIDCharacter(value[i]) {
			return false
		}
	}
	return true
}

func isULIDCharacter(value byte) bool {
	for i := range len(ulidAlphabet) {
		if value == ulidAlphabet[i] {
			return true
		}
	}
	return false
}

var operationRandom = rand.Reader
