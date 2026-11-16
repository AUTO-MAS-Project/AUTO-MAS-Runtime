package uv

import "testing"

func TestPOSIXAssets_TargetMatrix(t *testing.T) {
	for _, tc := range []struct{ os, arch, name, hash string }{
		{"linux", "amd64", "uv-x86_64-unknown-linux-gnu.tar.gz", "600cf9a742aca00d292673b16b5acffaa7b8c269a364ad0c2e79498dcb1fe101"},
		{"linux", "arm64", "uv-aarch64-unknown-linux-gnu.tar.gz", "bb66cb52e7b1823aed1183630d8d8e5c958840d584a4c55ec10a4cfc168dcca2"},
		{"darwin", "amd64", "uv-x86_64-apple-darwin.tar.gz", "4c9f52262a14da336e4a42ed24992d12d0c956acde87619e4611d321dffa602b"},
		{"darwin", "arm64", "uv-aarch64-apple-darwin.tar.gz", "546f7f8a6c70ff13a3a9d2bc958db3427298cebf3e0cb756f9177133b7068843"},
	} {
		t.Run(tc.os+"/"+tc.arch, func(t *testing.T) {
			artifact, err := ArtifactFor(tc.os, tc.arch)
			if err != nil || artifact.Name != tc.name || artifact.SHA256 != tc.hash {
				t.Fatalf("got %+v %v, want %s %s", artifact, err, tc.name, tc.hash)
			}
		})
	}
	if _, err := ArtifactFor("linux", "386"); err == nil {
		t.Fatal("got unsupported target, want error")
	}
}
