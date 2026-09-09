package filesystem

import (
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// VenvReport 是受管项目虚拟环境的结构完整性快照。
type VenvReport struct {
	// Intact 表示 venv 具备启动所需的最小结构。
	Intact bool
	// Missing 按检查顺序列出缺失或类型不对的路径，Intact 为 true 时为空。
	Missing []string
}

// InspectVenv 报告受管项目虚拟环境是否结构完整。
//
// 只做结构判断，不执行解释器：判据是 pyvenv.cfg 与 venv 内 python.exe 两个普通文件
// 同时存在。真机上出现过「目录还在、python.exe 还在，只有 pyvenv.cfg 被删掉」的残骸
// ——一次撞上后端进程未退尽的 venv 重建会删掉未被占用的文件、在被锁住的 python.exe 上
// 失败退出。此时 CPython 算不出 home，一启动就以 `No pyvenv.cfg file` 退出，而只查
// 目录是否存在的判据完全看不出问题，环境会被当成就绪一路交给后端启动。
func InspectVenv(layout *config.Layout) VenvReport {
	if layout == nil {
		return VenvReport{}
	}
	report := VenvReport{Intact: true}
	for _, path := range []string{layout.VenvConfigFile(), layout.VenvPythonExecutable()} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			report.Intact = false
			report.Missing = append(report.Missing, path)
		}
	}
	return report
}
