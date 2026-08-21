package gitrepo

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var (
	errInvalidCheckoutPolicy = errors.New("repository checkout policy is invalid")
	errCheckoutTreeEmpty     = errors.New("repository checkout tree is empty")
)

// checkoutPolicy 保存已经校验的仓库根目录排除项。
type checkoutPolicy struct {
	exclusions map[string]struct{}
}

// repositoryCheckoutExclusions 是受管仓库不需要检出的根目录文件和文件夹清单。
// 修改此处后必须运行 checkout policy 与 Git 组件测试。
func repositoryCheckoutExclusions() []string {
	return []string{
		"frontend",
		".debug",
	}
}

func newCheckoutPolicy(exclusions []string) (checkoutPolicy, error) {
	policy := checkoutPolicy{exclusions: make(map[string]struct{}, len(exclusions))}
	for _, exclusion := range exclusions {
		if invalidCheckoutExclusion(exclusion) || requiredCheckoutRootEntry(exclusion) {
			return checkoutPolicy{}, fmt.Errorf("%w: %q", errInvalidCheckoutPolicy, exclusion)
		}
		if _, exists := policy.exclusions[exclusion]; exists {
			return checkoutPolicy{}, fmt.Errorf("%w: duplicate %q", errInvalidCheckoutPolicy, exclusion)
		}
		policy.exclusions[exclusion] = struct{}{}
	}
	return policy, nil
}

func (p checkoutPolicy) valid() bool {
	return p.exclusions != nil
}

func (p checkoutPolicy) patterns(tree *object.Tree) ([]string, error) {
	if !p.valid() || tree == nil {
		return nil, errInvalidCheckoutPolicy
	}
	patterns := make([]string, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		if _, excluded := p.exclusions[entry.Name]; excluded {
			continue
		}
		pattern := entry.Name
		if entry.Mode == filemode.Dir {
			pattern += "/"
		}
		if p.patternIncludesExcludedEntry(pattern) {
			return nil, fmt.Errorf("%w: ambiguous root entry %q", errInvalidCheckoutPolicy, entry.Name)
		}
		patterns = append(patterns, pattern)
	}
	if len(patterns) == 0 {
		return nil, errCheckoutTreeEmpty
	}
	sort.Strings(patterns)
	return patterns, nil
}

func (p checkoutPolicy) patternIncludesExcludedEntry(pattern string) bool {
	for exclusion := range p.exclusions {
		if strings.HasPrefix(exclusion, pattern) {
			return true
		}
	}
	return false
}

func invalidCheckoutExclusion(exclusion string) bool {
	return exclusion == "" ||
		exclusion == "." ||
		exclusion == ".." ||
		exclusion == ".git" ||
		strings.ContainsAny(exclusion, `/\*?[]:`)
}

func requiredCheckoutRootEntry(entry string) bool {
	switch entry {
	case ".python-version", "pyproject.toml", "uv.lock", "main.py", "app", "res":
		return true
	default:
		return false
	}
}
