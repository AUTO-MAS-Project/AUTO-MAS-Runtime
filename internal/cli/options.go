package cli

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

// versionSourceFunc 是 version 命令与 hello 版本字段的来源。
type versionSourceFunc func(context.Context) (version.Info, error)

// WithVersionSource 注入版本信息来源，默认使用 version.Load。
func WithVersionSource(source versionSourceFunc) Option {
	return func(values *options) error {
		if source == nil {
			return errors.New("cli version source must not be nil")
		}
		values.versionSource = source
		return nil
	}
}
