// Package lock 按物理 app root 协调 Runtime 操作。Windows 使用命名 Mutex，macOS 使用持久 flock 文件。
package lock
