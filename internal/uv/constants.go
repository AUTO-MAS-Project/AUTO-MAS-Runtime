package uv

import "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"

const (
	// FixedVersion 是 Runtime 首版固定使用的 uv 版本。
	FixedVersion = "0.12.3"
	// WindowsX64Artifact 是 Windows x64 官方发行文件名。
	WindowsX64Artifact = "uv-x86_64-pc-windows-msvc.zip"
	// WindowsX64SHA256 是 Windows x64 发行文件的固定 SHA-256。
	WindowsX64SHA256 = "b23350c79e8ad0192b8124af13a0f17e8d4e4549524785e1aef389ae5a06990e"
)

// Artifact 描述当前平台的固定 uv 制品。
type Artifact struct {
	Version string
	Kind    mirror.Kind
	Name    string
	SHA256  string
}

// WindowsX64ArtifactSpec 返回 Windows x64 的防御性制品描述。
func WindowsX64ArtifactSpec() Artifact {
	return Artifact{
		Version: FixedVersion,
		Kind:    mirror.KindUV,
		Name:    WindowsX64Artifact,
		SHA256:  WindowsX64SHA256,
	}
}
