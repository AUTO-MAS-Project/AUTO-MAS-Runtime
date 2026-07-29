package config

import (
	"errors"
	"strings"
	"testing"
)

func TestAppendPartSuffix_UTF16LengthBoundary(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{
			name:  "250 ASCII plus suffix is 255 units",
			value: strings.Repeat("a", 250),
			want:  strings.Repeat("a", 250) + ".part",
		},
		{
			name:    "251 ASCII plus suffix is 256 units",
			value:   strings.Repeat("a", 251),
			wantErr: true,
		},
		{
			name:  "125 supplementary runes plus suffix is 255 units",
			value: strings.Repeat("\U0001F600", 125),
			want:  strings.Repeat("\U0001F600", 125) + ".part",
		},
		{
			name:    "126 supplementary runes plus suffix is 257 units",
			value:   strings.Repeat("\U0001F600", 126),
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := appendPartSuffix(test.value)
			if test.wantErr {
				if got != "" {
					t.Fatalf("appendPartSuffix() = %q, want empty string", got)
				}
				if !errors.Is(err, ErrInvalidSegment) {
					t.Fatalf("appendPartSuffix() error = %v, want errors.Is(_, %v)", err, ErrInvalidSegment)
				}
				return
			}
			if err != nil {
				t.Fatalf("appendPartSuffix() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("appendPartSuffix() = %q, want %q", got, test.want)
			}
		})
	}
}
