package version

import "strings"

// ProductBranch 固定产品版本的后端分支；调用方仍需校验安全版本字符。
// 仅规范 alpha 版本进入滚动 dev，历史非标准版本继续按 release 规则定位。
func ProductBranch(productVersion string) string {
	base, run, alpha := strings.Cut(productVersion, "-alpha.")
	if alpha && strings.HasPrefix(base, "v") && canonicalNumber(run) {
		parts := strings.Split(strings.TrimPrefix(base, "v"), ".")
		if len(parts) == 3 && canonicalNumber(parts[0]) && canonicalNumber(parts[1]) && canonicalNumber(parts[2]) {
			return "dev"
		}
	}
	return "release/" + productVersion
}

func canonicalNumber(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
