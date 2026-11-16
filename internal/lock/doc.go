// Package lock 按物理 app root 协调 Runtime 操作。
// Windows 使用命名 Mutex；POSIX 使用持久文件锁并复核锁文件身份。
package lock
