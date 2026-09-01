// Package process 负责 Windows 进程与 Job Object 集成。
//
// Runtime 创建的进程及其后代默认全部留在同一个 Job Object 内，由它统一回收；
// Job 同时开启 BREAKAWAY_OK，因此**显式**带 CREATE_BREAKAWAY_FROM_JOB 创建的
// 进程会脱离该边界、不随 Job 关闭被杀（增补 1 C8：模拟器与 PC 游戏）。
// 脱离出去的进程的生命周期归创建它的一方负责，Runtime 既不再回收它，也不会
// 按进程名去找它。
package process
