package uv

import "testing"

// TestMarker_EvaluatesSubset 覆盖增补 2 C18 第 3 条限定的 marker 子集：
// 比较、in / not in、and / or、括号，以及 python 版本的 PEP 440 简单比较。
func TestMarker_EvaluatesSubset(t *testing.T) {
	t.Parallel()

	env := windowsMarkerEnvironment(PythonVersion{Major: 3, Minor: 12, Patch: 13}).withExtra("socks")
	tests := []struct {
		name    string
		marker  string
		want    bool
		wantErr bool
	}{
		{name: "win32 equals", marker: "sys_platform == 'win32'", want: true},
		{name: "darwin equals", marker: "sys_platform == 'darwin'", want: false},
		{name: "not emscripten", marker: "sys_platform != 'emscripten'", want: true},
		{name: "substring in", marker: "'linux' in sys_platform", want: false},
		{name: "substring in win", marker: "'win' in sys_platform", want: true},
		{name: "not in", marker: "'linux' not in sys_platform", want: true},
		{name: "implementation", marker: "implementation_name != 'PyPy'", want: true},
		{name: "platform implementation", marker: "platform_python_implementation != 'PyPy'", want: true},
		{name: "machine", marker: "platform_machine == 'aarch64' and sys_platform == 'linux'", want: false},
		{name: "or with parens", marker: "(platform_machine != 'aarch64' and sys_platform == 'linux') or (sys_platform != 'darwin' and sys_platform != 'linux')", want: true},
		{name: "python version ge", marker: "python_version >= '3.10'", want: true},
		{name: "python version lt", marker: "python_version < '3.10'", want: false},
		{name: "python full version", marker: "python_full_version >= '3.12.4'", want: true},
		{name: "python version numeric not lexical", marker: "python_version >= '3.9'", want: true},
		{name: "extra matches", marker: "extra == 'socks'", want: true},
		{name: "extra mismatch", marker: "extra == 'http2'", want: false},
		{name: "double quotes", marker: `sys_platform == "win32"`, want: true},
		{name: "unknown variable", marker: "platform_flavor == 'x'", wantErr: true},
		{name: "garbage", marker: "sys_platform ==", wantErr: true},
		{name: "trailing tokens", marker: "sys_platform == 'win32' extra", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateMarker(test.marker, env)
			if test.wantErr {
				if err == nil {
					t.Fatalf("evaluateMarker(%q) error = nil, want error", test.marker)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluateMarker(%q) error = %v", test.marker, err)
			}
			if got != test.want {
				t.Errorf("evaluateMarker(%q) = %v, want %v", test.marker, got, test.want)
			}
		})
	}
}
