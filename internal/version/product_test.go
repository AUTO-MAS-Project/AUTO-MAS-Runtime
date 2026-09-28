package version

import "testing"

func TestProductBranch_Channels(t *testing.T) {
	tests := []struct {
		name    string
		version string
		branch  string
	}{
		{name: "alpha", version: "v5.6.0-alpha.123", branch: "dev"},
		{name: "alpha zero", version: "v0.0.0-alpha.0", branch: "dev"},
		{name: "beta", version: "v5.7.0-beta.2", branch: "release/v5.7.0-beta.2"},
		{name: "stable patch", version: "v5.6.1", branch: "release/v5.6.1"},
		{name: "historical", version: "v5.2.0-withplugin.0.0.1", branch: "release/v5.2.0-withplugin.0.0.1"},
		{name: "unrelated alpha", version: "v5.6.0-alphabeta.1", branch: "release/v5.6.0-alphabeta.1"},
		{name: "non numeric alpha", version: "v5.6.0-alpha.fake", branch: "release/v5.6.0-alpha.fake"},
		{name: "leading zero", version: "v5.6.0-alpha.01", branch: "release/v5.6.0-alpha.01"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ProductBranch(test.version); got != test.branch {
				t.Fatalf("ProductBranch(%q) = %q, want %q", test.version, got, test.branch)
			}
		})
	}
}
