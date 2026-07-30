package mirror

// Kind 标识互不混用的网络源类别。
type Kind string

const (
	// KindGit 表示后端 Git 仓库源。
	KindGit Kind = "git"
	// KindUV 表示 uv 二进制发布源。
	KindUV Kind = "uv"
	// KindPython 表示 Python standalone 分发源。
	KindPython Kind = "python"
	// KindPackageIndex 表示主项目 Python 包索引。
	KindPackageIndex Kind = "package-index"
)

// AllKinds 按冻结顺序返回全部 Kind。
func AllKinds() []Kind {
	return []Kind{
		KindGit,
		KindUV,
		KindPython,
		KindPackageIndex,
	}
}

// String 返回 Kind 的稳定字面量。
func (k Kind) String() string {
	return string(k)
}

// Valid 报告 Kind 是否属于冻结全集。
func (k Kind) Valid() bool {
	switch k {
	case KindGit, KindUV, KindPython, KindPackageIndex:
		return true
	default:
		return false
	}
}

func validSourceKey(value string) bool {
	if value == "" {
		return false
	}
	previousHyphen := false
	for i := 0; i < len(value); i++ {
		character := value[i]
		switch {
		case character >= 'a' && character <= 'z':
			previousHyphen = false
		case character >= '0' && character <= '9':
			if i == 0 {
				return false
			}
			previousHyphen = false
		case character == '-':
			if i == 0 || i == len(value)-1 || previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return true
}
