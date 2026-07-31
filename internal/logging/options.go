package logging

import (
	"fmt"
	"time"
)

// RetentionPolicy 定义本地日历日和每个命令的日志文件上限。
type RetentionPolicy struct {
	MaxAgeDays         int
	MaxFilesPerCommand int
}

type options struct {
	clock     func() time.Time
	retention RetentionPolicy
}

// Option 配置 Logger 构造参数。
type Option func(*options) error

// DefaultRetentionPolicy 返回冻结的 30 日、每命令 30 文件策略。
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 30}
}

// WithClock 注入返回本地时间的时钟。
func WithClock(clock func() time.Time) Option {
	return func(values *options) error {
		if clock == nil {
			return fmt.Errorf("configure logging clock: %w", ErrInvalidArgument)
		}
		values.clock = clock
		return nil
	}
}

// WithRetention 按值复制保留策略。
func WithRetention(policy RetentionPolicy) Option {
	return func(values *options) error {
		if err := validateRetention(policy); err != nil {
			return err
		}
		values.retention = policy
		return nil
	}
}

func defaultOptions() options {
	return options{
		clock:     time.Now,
		retention: DefaultRetentionPolicy(),
	}
}

func applyOptions(values ...Option) (options, error) {
	applied := defaultOptions()
	for index, option := range values {
		if option == nil {
			return options{}, fmt.Errorf("apply logging option %d: %w", index, ErrInvalidArgument)
		}
		if err := option(&applied); err != nil {
			return options{}, fmt.Errorf("apply logging option %d: %w", index, err)
		}
	}
	return applied, nil
}

func validateRetention(policy RetentionPolicy) error {
	if policy.MaxAgeDays <= 0 || policy.MaxFilesPerCommand <= 0 {
		return fmt.Errorf("validate logging retention: %w", ErrInvalidRetention)
	}
	return nil
}
