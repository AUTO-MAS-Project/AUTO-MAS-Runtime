package version

import (
	"context"
	"runtime"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var (
	// Version 是 Runtime 发布版本，构建期通过 -ldflags -X 注入。
	Version = "dev"
	// Commit 是构建对应的提交哈希，可为空字符串。
	Commit = ""
	// BuildDate 是构建时间，可为空字符串。
	BuildDate = ""
)

// Info 是 version 命令与 hello 共享的版本信息快照。
type Info struct {
	Version   string
	Protocol  int
	Commit    string
	BuildDate string
	GoVersion string
}

// Load 返回当前构建的版本信息；生产路径不会失败。
func Load(ctx context.Context) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	return Info{
		Version:   Version,
		Protocol:  protocol.Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
	}, nil
}
