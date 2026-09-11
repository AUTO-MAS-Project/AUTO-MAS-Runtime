package uv

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// officialArtifactHost 是锁内 registry 制品 URL 的官方主机；规划器只认它之下的路径。
const officialArtifactHost = "files.pythonhosted.org"

// LockArtifact 是锁内为本平台选定的一个制品：PyPI 原路径（不含 `/packages/` 前缀）、大小与十六进制 sha256。
type LockArtifact struct {
	Package string
	Path    string
	Size    int64
	SHA256  string
}

// LockPlan 是从锁文件规划出的本平台可安装制品集合（增补 2 C18 第 3 条）。
//
// 它只用于进度总量与探针目标，不参与安装决策：uv 仍按锁自行选择制品，
// 因此 Skipped 记录的是求值不了、按可达处理的 marker 数，方向只会高估。
type LockPlan struct {
	Items      []LockArtifact
	TotalBytes int64
	Largest    LockArtifact
	Skipped    int
}

var errLockPlan = errors.New("lock plan is invalid")

type lockDocument struct {
	Packages []lockPackage `toml:"package"`
}

type lockPackage struct {
	Name                 string                      `toml:"name"`
	Source               map[string]string           `toml:"source"`
	Dependencies         []lockDependency            `toml:"dependencies"`
	OptionalDependencies map[string][]lockDependency `toml:"optional-dependencies"`
	Sdist                *lockArtifactEntry          `toml:"sdist"`
	Wheels               []lockArtifactEntry         `toml:"wheels"`
}

type lockDependency struct {
	Name   string   `toml:"name"`
	Marker string   `toml:"marker"`
	Extra  []string `toml:"extra"`
}

type lockArtifactEntry struct {
	URL  string `toml:"url"`
	Hash string `toml:"hash"`
	Size int64  `toml:"size"`
}

// PlanLock 解析锁文本，从根项目（source 为 virtual / editable "."）的 dependencies 出发，
// 按 marker 求值收集可达包，并为每个包选出本平台制品。
func PlanLock(lock string, pythonVersion PythonVersion) (LockPlan, error) {
	var document lockDocument
	if _, err := toml.Decode(lock, &document); err != nil {
		return LockPlan{}, fmt.Errorf("%w: %w", errLockPlan, err)
	}
	packages := make(map[string]*lockPackage, len(document.Packages))
	var root *lockPackage
	for index := range document.Packages {
		entry := &document.Packages[index]
		if entry.Name == "" {
			return LockPlan{}, fmt.Errorf("%w: package without name", errLockPlan)
		}
		if isRootSource(entry.Source) {
			if root != nil {
				return LockPlan{}, fmt.Errorf("%w: multiple root packages", errLockPlan)
			}
			root = entry
			continue
		}
		// 同名多版本（不同分叉）在本平台锁里不会出现；出现时保留第一个，方向仍是高估。
		if _, exists := packages[entry.Name]; !exists {
			packages[entry.Name] = entry
		}
	}
	if root == nil {
		return LockPlan{}, fmt.Errorf("%w: root package is missing", errLockPlan)
	}

	env := windowsMarkerEnvironment(pythonVersion)
	walker := &lockWalker{packages: packages, env: env, visited: make(map[string]map[string]struct{})}
	walker.visit(root, "", root.Dependencies)

	plan := LockPlan{Skipped: walker.skipped}
	names := make([]string, 0, len(walker.reached))
	for name := range walker.reached {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := packages[name]
		artifact, ok, err := selectArtifact(entry, pythonVersion)
		if err != nil {
			return LockPlan{}, err
		}
		if !ok {
			continue
		}
		plan.Items = append(plan.Items, artifact)
		plan.TotalBytes += artifact.Size
		if artifact.Size > plan.Largest.Size {
			plan.Largest = artifact
		}
	}
	return plan, nil
}

func isRootSource(source map[string]string) bool {
	for _, key := range []string{"virtual", "editable", "directory"} {
		if value, ok := source[key]; ok && value == "." {
			return true
		}
	}
	return false
}

// lockWalker 沿依赖边做可达性遍历；visited 以「包名 → 已展开的 extra 集合」记录，避免 extra 边被重复或遗漏。
type lockWalker struct {
	packages map[string]*lockPackage
	env      markerEnvironment
	reached  map[string]struct{}
	visited  map[string]map[string]struct{}
	skipped  int
}

func (w *lockWalker) visit(from *lockPackage, extra string, edges []lockDependency) {
	if w.reached == nil {
		w.reached = make(map[string]struct{})
	}
	key := extra
	if key == "" {
		key = "\x00"
	}
	expanded, ok := w.visited[from.Name]
	if !ok {
		expanded = make(map[string]struct{})
		w.visited[from.Name] = expanded
	}
	if _, done := expanded[key]; done {
		return
	}
	expanded[key] = struct{}{}

	edgeEnv := w.env
	if extra != "" {
		edgeEnv = w.env.withExtra(extra)
	}
	for _, edge := range edges {
		if edge.Marker != "" {
			reachable, err := evaluateMarker(edge.Marker, edgeEnv)
			if err != nil {
				w.skipped++
			} else if !reachable {
				continue
			}
		}
		target, exists := w.packages[edge.Name]
		if !exists {
			// 根包不会被依赖；指向锁外的名字说明锁本身有问题，按不可达处理。
			continue
		}
		w.reached[target.Name] = struct{}{}
		w.visit(target, "", target.Dependencies)
		for _, requestedExtra := range edge.Extra {
			w.visit(target, requestedExtra, target.OptionalDependencies[requestedExtra])
		}
	}
}

// selectArtifact 按增补 2 C18 第 3 条选制品：cp312 > abi3 > py3 的 win_amd64 wheel，其次 none-any，都没有取 sdist。
func selectArtifact(entry *lockPackage, pythonVersion PythonVersion) (LockArtifact, bool, error) {
	bestScore := 0
	var best *lockArtifactEntry
	for index := range entry.Wheels {
		wheel := &entry.Wheels[index]
		score := wheelScore(artifactFileName(wheel.URL), pythonVersion)
		if score > bestScore {
			bestScore = score
			best = wheel
		}
	}
	if best == nil {
		if entry.Sdist == nil || entry.Sdist.URL == "" {
			return LockArtifact{}, false, nil
		}
		best = entry.Sdist
	}
	artifact, err := toLockArtifact(entry.Name, best)
	if err != nil {
		return LockArtifact{}, false, err
	}
	return artifact, true, nil
}

func artifactFileName(rawURL string) string {
	if index := strings.LastIndexByte(rawURL, '/'); index >= 0 {
		return rawURL[index+1:]
	}
	return rawURL
}

// wheelScore 给 wheel 文件名打分，0 表示与 Windows x64 CPython 不兼容。
func wheelScore(fileName string, pythonVersion PythonVersion) int {
	if !strings.HasSuffix(fileName, ".whl") {
		return 0
	}
	parts := strings.Split(strings.TrimSuffix(fileName, ".whl"), "-")
	if len(parts) < 5 {
		return 0
	}
	pythonTags := strings.Split(parts[len(parts)-3], ".")
	abiTags := strings.Split(parts[len(parts)-2], ".")
	platformTags := strings.Split(parts[len(parts)-1], ".")

	platformScore := 0
	for _, tag := range platformTags {
		switch tag {
		case "win_amd64":
			platformScore = max(platformScore, 2)
		case "any":
			platformScore = max(platformScore, 1)
		}
	}
	if platformScore == 0 {
		return 0
	}
	cpython := fmt.Sprintf("cp%d%d", pythonVersion.Major, pythonVersion.Minor)
	abiScore := 0
	for _, tag := range abiTags {
		switch tag {
		case cpython:
			abiScore = max(abiScore, 3)
		case "abi3":
			abiScore = max(abiScore, 2)
		case "none":
			abiScore = max(abiScore, 1)
		}
	}
	if abiScore == 0 {
		return 0
	}
	pythonScore := 0
	for _, tag := range pythonTags {
		switch {
		case tag == cpython, tag == fmt.Sprintf("py%d%d", pythonVersion.Major, pythonVersion.Minor):
			pythonScore = max(pythonScore, 2)
		case tag == fmt.Sprintf("py%d", pythonVersion.Major):
			pythonScore = max(pythonScore, 1)
		case strings.HasPrefix(tag, "cp") && abiScore == 2:
			// abi3 wheel 的 python tag 是最低支持版本（cp37-abi3），只要不高于当前版本即可。
			if minor, ok := cpythonMinor(tag); ok && minor <= pythonVersion.Minor {
				pythonScore = max(pythonScore, 1)
			}
		}
	}
	if pythonScore == 0 {
		return 0
	}
	return platformScore*100 + abiScore*10 + pythonScore
}

func cpythonMinor(tag string) (int, bool) {
	digits := strings.TrimPrefix(tag, "cp")
	if len(digits) < 2 || digits[0] != '3' {
		return 0, false
	}
	minor := 0
	for _, character := range digits[1:] {
		if character < '0' || character > '9' {
			return 0, false
		}
		minor = minor*10 + int(character-'0')
	}
	return minor, true
}

func toLockArtifact(name string, entry *lockArtifactEntry) (LockArtifact, error) {
	parsed, err := url.Parse(entry.URL)
	if err != nil || parsed.Host != officialArtifactHost || !strings.HasPrefix(parsed.Path, "/packages/") {
		return LockArtifact{}, fmt.Errorf("%w: artifact url %q is not an official pypi url", errLockPlan, entry.URL)
	}
	digest, ok := strings.CutPrefix(entry.Hash, "sha256:")
	if !ok || len(digest) != 64 {
		return LockArtifact{}, fmt.Errorf("%w: artifact %q has no sha256", errLockPlan, entry.URL)
	}
	if entry.Size <= 0 {
		return LockArtifact{}, fmt.Errorf("%w: artifact %q has no size", errLockPlan, entry.URL)
	}
	return LockArtifact{
		Package: name,
		Path:    strings.TrimPrefix(parsed.Path, "/packages/"),
		Size:    entry.Size,
		SHA256:  strings.ToLower(digest),
	}, nil
}
