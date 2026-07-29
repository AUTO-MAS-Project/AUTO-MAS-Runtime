// Package protocol 定义单次 Runtime 操作的事件、错误、生命周期、渲染和 stdin 控制契约。
//
// ProcessOutput 通过同一个 renderer 串行化所有事件，只允许一个终态 result，
// 并拒绝其后的任何事件。成功发射的 warning 会以有界快照形式汇总到权威 result 中。
//
// contracttest 子包为命令集成测试校验原始 NDJSON 分帧、公共结构、终态语义与 warning 汇总。
package protocol
