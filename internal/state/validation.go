package state

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

const (
	maxProductVersionBytes = 128
	maxToolVersionBytes    = 128
	maxLogPathBytes        = 32767
)

const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func validateOperationID(value string) error {
	if len(value) != 26 || value[0] > '7' {
		return validationError("operationId")
	}
	for i := 0; i < len(value); i++ {
		if !strings.ContainsRune(ulidAlphabet, rune(value[i])) {
			return validationError("operationId")
		}
	}
	return nil
}

func validateProductVersion(value string) error {
	if len(value) < 2 || len(value) > maxProductVersionBytes || value[0] != 'v' {
		return validationError("version")
	}
	if strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.HasSuffix(value, ".") {
		return validationError("version")
	}
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if !isASCIIAlphaNumeric(ch) && ch != '.' && ch != '-' && ch != '_' {
			return validationError("version")
		}
	}
	return nil
}

func validateToolVersion(value string) error {
	if len(value) == 0 || len(value) > maxToolVersionBytes ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.HasSuffix(value, ".") {
		return validationError("toolVersion")
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if !isASCIIAlphaNumeric(ch) && ch != '.' && ch != '-' && ch != '_' {
			return validationError("toolVersion")
		}
	}
	return nil
}

func validateCommit(value string) error {
	if len(value) != 40 {
		return validationError("commit")
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return validationError("commit")
		}
	}
	return nil
}

func validateTimestamp(field string, value time.Time) error {
	if value.IsZero() {
		return validationError(field)
	}
	if _, err := value.MarshalText(); err != nil {
		return validationError(field)
	}
	return nil
}

func validateRuntimeLogPath(layout *config.Layout, value string) error {
	if layout == nil || value == "" || len(value) > maxLogPathBytes ||
		!utf8.ValidString(value) || containsControl(value) || !filepath.IsAbs(value) {
		return validationError("logPath")
	}
	relative, err := filepath.Rel(layout.RuntimeLogDir(), filepath.Clean(value))
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return validationError("logPath")
	}
	return nil
}

func containsControl(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}

func isASCIIAlphaNumeric(ch byte) bool {
	return ch >= '0' && ch <= '9' ||
		ch >= 'A' && ch <= 'Z' ||
		ch >= 'a' && ch <= 'z'
}
