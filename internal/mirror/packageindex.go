package mirror

import (
	"fmt"
	"strings"
)

// PackageIndexSpec 是带显式锁改写前缀的包索引源构造输入。
type PackageIndexSpec struct {
	Key          string
	BaseURL      string
	SimpleBase   string
	PackagesBase string
	Official     bool
}

// PackageIndexRewrite 保存包索引源在锁文件 URL 改写中使用的两个前缀。
//
// 两个前缀由目录显式声明，不从 BaseURL 推导：官方源的 artifact 位于
// files.pythonhosted.org，与它的索引不同 host，任何「去掉结尾 simple/」
// 一类的推导都会得到不存在的地址；三家镜像索引与 artifact 同 host 只是巧合。
type PackageIndexRewrite struct {
	simpleBase   string
	packagesBase string
}

// SimpleBase 返回替换 https://pypi.org/simple 的前缀，不以 / 结尾。
func (r PackageIndexRewrite) SimpleBase() string {
	return r.simpleBase
}

// PackagesBase 返回替换 https://files.pythonhosted.org/packages/ 的前缀，以 / 结尾。
func (r PackageIndexRewrite) PackagesBase() string {
	return r.packagesBase
}

// NewPackageIndexSource 校验并构造带显式改写前缀的包索引源。
func NewPackageIndexSource(spec PackageIndexSpec) (Source, error) {
	source, err := NewSource(KindPackageIndex, spec.Key, spec.BaseURL, spec.Official)
	if err != nil {
		return Source{}, err
	}
	simpleBase, packagesBase, err := normalizeRewritePrefixes(spec.SimpleBase, spec.PackagesBase)
	if err != nil {
		return Source{}, err
	}
	source.simpleBase = simpleBase
	source.packagesBase = packagesBase
	return source, nil
}

// PackageIndexRewrite 返回包索引源的锁改写前缀；未显式声明时报告 false。
func (s Source) PackageIndexRewrite() (PackageIndexRewrite, bool) {
	if s.kind != KindPackageIndex || s.simpleBase == "" || s.packagesBase == "" {
		return PackageIndexRewrite{}, false
	}
	return PackageIndexRewrite{simpleBase: s.simpleBase, packagesBase: s.packagesBase}, true
}

// normalizeRewritePrefixes 校验两个前缀的 HTTPS 策略与尾斜杠不变量。
//
// 尾斜杠是硬约束：simple 前缀替换的原串 https://pypi.org/simple 不带斜杠，
// packages 前缀之后要直接拼 PyPI 原路径，因此必须带斜杠。前缀还必须已经是
// 规范形式，避免目录里写的值与实际参与改写的值不是同一个串。
func normalizeRewritePrefixes(simpleBase, packagesBase string) (string, string, error) {
	if simpleBase == "" || packagesBase == "" ||
		strings.HasSuffix(simpleBase, "/") ||
		!strings.HasSuffix(packagesBase, "/") {
		return "", "", fmt.Errorf("%w: package index rewrite prefix shape", errInvalidSource)
	}
	normalizedSimple, err := normalizeSourceURL(simpleBase)
	if err != nil {
		return "", "", err
	}
	normalizedPackages, err := normalizeSourceURL(packagesBase)
	if err != nil {
		return "", "", err
	}
	if normalizedSimple != simpleBase || normalizedPackages != packagesBase {
		return "", "", fmt.Errorf("%w: package index rewrite prefix is not canonical", errInvalidSource)
	}
	return normalizedSimple, normalizedPackages, nil
}
