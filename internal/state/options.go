package state

import "time"

// Option 配置 Store 的可测试依赖。
type Option func(*options) error

type options struct {
	clock    func() time.Time
	clockSet bool
}

// WithClock 注入每次状态构造使用一次的时钟。
func WithClock(clock func() time.Time) Option {
	return func(values *options) error {
		if clock == nil || values.clockSet {
			return validationError("clock")
		}
		values.clock = clock
		values.clockSet = true
		return nil
	}
}

func applyOptions(values ...Option) (options, error) {
	result := options{clock: time.Now}
	for _, option := range values {
		if option == nil {
			return options{}, validationError("option")
		}
		if err := option(&result); err != nil {
			return options{}, err
		}
	}
	return result, nil
}
