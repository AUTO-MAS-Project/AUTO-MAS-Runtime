package logging

import "context"

func (l *Logger) recordFileSystem(ctx context.Context, probe func(context.Context, string) (string, error)) error {
	path := l.layout.AppRoot()
	format, err := probe(ctx, path)
	if err != nil || format == "" {
		// 格式只用于排障，探测不可用不能成为安装或启动的新前置条件。
		format = "unknown"
	}
	_, err = l.Diagnostic(ctx, LevelInfo, "当前磁盘格式："+format, map[string]any{
		"path":       path,
		"fileSystem": format,
	})
	return err
}
