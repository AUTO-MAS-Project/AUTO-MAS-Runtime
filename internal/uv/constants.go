package uv

import (
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

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

// ArtifactFor 返回冻结的 OS/架构官方资产；摘要来自 uv 0.12.3 的 sha256.sum。
func ArtifactFor(goos, goarch string) (Artifact, error) {
	if goos == "windows" && goarch == "amd64" {
		return WindowsX64ArtifactSpec(), nil
	}
	type asset struct{ name, hash string }
	assets := map[string]asset{
		"linux/amd64":  {"uv-x86_64-unknown-linux-gnu.tar.gz", "600cf9a742aca00d292673b16b5acffaa7b8c269a364ad0c2e79498dcb1fe101"},
		"linux/arm64":  {"uv-aarch64-unknown-linux-gnu.tar.gz", "bb66cb52e7b1823aed1183630d8d8e5c958840d584a4c55ec10a4cfc168dcca2"},
		"darwin/amd64": {"uv-x86_64-apple-darwin.tar.gz", "4c9f52262a14da336e4a42ed24992d12d0c956acde87619e4611d321dffa602b"},
		"darwin/arm64": {"uv-aarch64-apple-darwin.tar.gz", "546f7f8a6c70ff13a3a9d2bc958db3427298cebf3e0cb756f9177133b7068843"},
	}
	value, ok := assets[goos+"/"+goarch]
	if !ok {
		return Artifact{}, fmt.Errorf("unsupported uv target %s/%s", goos, goarch)
	}
	return Artifact{Version: FixedVersion, Kind: mirror.KindUV, Name: value.name, SHA256: value.hash}, nil
}
