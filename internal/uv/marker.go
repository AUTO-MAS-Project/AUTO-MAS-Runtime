package uv

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// markerEnvironment 是求值 PEP 508 marker 时的固定环境。
//
// 规划器只服务 Windows x64 CPython 的进度总量估算（增补 2 C18 第 3 条），
// 因此环境写死，不从宿主机读取任何值。
type markerEnvironment struct {
	values map[string]string
}

// windowsMarkerEnvironment 返回受管 Windows x64 CPython 的 marker 环境。
func windowsMarkerEnvironment(version PythonVersion) markerEnvironment {
	full := version.String()
	return markerEnvironment{values: map[string]string{
		"os_name":                        "nt",
		"sys_platform":                   "win32",
		"platform_machine":               "AMD64",
		"platform_system":                "Windows",
		"platform_release":               "10",
		"platform_version":               "10.0",
		"platform_python_implementation": "CPython",
		"implementation_name":            "cpython",
		"implementation_version":         full,
		"python_version":                 fmt.Sprintf("%d.%d", version.Major, version.Minor),
		"python_full_version":            full,
		"extra":                          "",
	}}
}

// withExtra 返回带指定 extra 的副本；空串表示无 extra。
func (e markerEnvironment) withExtra(extra string) markerEnvironment {
	values := make(map[string]string, len(e.values))
	for key, value := range e.values {
		values[key] = value
	}
	values["extra"] = extra
	return markerEnvironment{values: values}
}

// versionVariables 是按 PEP 440 简单比较而不是字典序比较的变量。
var versionVariables = map[string]struct{}{
	"python_version":         {},
	"python_full_version":    {},
	"implementation_version": {},
	"platform_version":       {},
	"platform_release":       {},
}

var errMarkerSyntax = errors.New("marker syntax is invalid")

// evaluateMarker 在给定环境下求值 marker 子集：==、!=、<、<=、>、>=、in、not in、and、or 与括号。
// 未知变量或语法错误返回错误，由规划器按可达处理并计数。
func evaluateMarker(marker string, env markerEnvironment) (bool, error) {
	tokens, err := tokenizeMarker(marker)
	if err != nil {
		return false, err
	}
	parser := &markerParser{tokens: tokens, env: env}
	value, err := parser.parseOr()
	if err != nil {
		return false, err
	}
	if parser.position != len(parser.tokens) {
		return false, fmt.Errorf("%w: trailing tokens", errMarkerSyntax)
	}
	return value, nil
}

type markerTokenKind int

const (
	markerTokenIdentifier markerTokenKind = iota
	markerTokenString
	markerTokenOperator
	markerTokenOpen
	markerTokenClose
)

type markerToken struct {
	kind  markerTokenKind
	value string
}

func tokenizeMarker(marker string) ([]markerToken, error) {
	tokens := make([]markerToken, 0, 8)
	for index := 0; index < len(marker); {
		character := marker[index]
		switch {
		case character == ' ' || character == '\t' || character == '\n' || character == '\r':
			index++
		case character == '(':
			tokens = append(tokens, markerToken{kind: markerTokenOpen})
			index++
		case character == ')':
			tokens = append(tokens, markerToken{kind: markerTokenClose})
			index++
		case character == '\'' || character == '"':
			end := strings.IndexByte(marker[index+1:], character)
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated string", errMarkerSyntax)
			}
			tokens = append(tokens, markerToken{kind: markerTokenString, value: marker[index+1 : index+1+end]})
			index += end + 2
		case markerOperatorPrefix(marker[index:]) != "":
			operator := markerOperatorPrefix(marker[index:])
			tokens = append(tokens, markerToken{kind: markerTokenOperator, value: operator})
			index += len(operator)
		case isMarkerIdentifierByte(character):
			end := index
			for end < len(marker) && isMarkerIdentifierByte(marker[end]) {
				end++
			}
			word := marker[index:end]
			switch word {
			case "in", "not", "and", "or":
				tokens = append(tokens, markerToken{kind: markerTokenOperator, value: word})
			default:
				tokens = append(tokens, markerToken{kind: markerTokenIdentifier, value: word})
			}
			index = end
		default:
			return nil, fmt.Errorf("%w: unexpected character %q", errMarkerSyntax, character)
		}
	}
	return tokens, nil
}

// markerOperatorPrefix 返回 rest 开头的比较运算符；最长匹配优先，没有则返回空串。
func markerOperatorPrefix(rest string) string {
	for _, operator := range []string{"===", "==", "!=", "<=", ">=", "<", ">"} {
		if strings.HasPrefix(rest, operator) {
			return operator
		}
	}
	return ""
}

func isMarkerIdentifierByte(character byte) bool {
	return character == '_' || character == '.' ||
		(character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z') ||
		(character >= '0' && character <= '9')
}

type markerParser struct {
	tokens   []markerToken
	position int
	env      markerEnvironment
}

func (p *markerParser) peek() (markerToken, bool) {
	if p.position >= len(p.tokens) {
		return markerToken{}, false
	}
	return p.tokens[p.position], true
}

func (p *markerParser) acceptOperator(value string) bool {
	token, ok := p.peek()
	if ok && token.kind == markerTokenOperator && token.value == value {
		p.position++
		return true
	}
	return false
}

func (p *markerParser) parseOr() (bool, error) {
	left, err := p.parseAnd()
	if err != nil {
		return false, err
	}
	for p.acceptOperator("or") {
		right, err := p.parseAnd()
		if err != nil {
			return false, err
		}
		left = left || right
	}
	return left, nil
}

func (p *markerParser) parseAnd() (bool, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return false, err
	}
	for p.acceptOperator("and") {
		right, err := p.parsePrimary()
		if err != nil {
			return false, err
		}
		left = left && right
	}
	return left, nil
}

func (p *markerParser) parsePrimary() (bool, error) {
	token, ok := p.peek()
	if !ok {
		return false, fmt.Errorf("%w: unexpected end", errMarkerSyntax)
	}
	if token.kind == markerTokenOpen {
		p.position++
		value, err := p.parseOr()
		if err != nil {
			return false, err
		}
		if next, ok := p.peek(); !ok || next.kind != markerTokenClose {
			return false, fmt.Errorf("%w: missing closing parenthesis", errMarkerSyntax)
		}
		p.position++
		return value, nil
	}
	left, leftIsVariable, err := p.parseValue()
	if err != nil {
		return false, err
	}
	operator, err := p.parseComparisonOperator()
	if err != nil {
		return false, err
	}
	right, rightIsVariable, err := p.parseValue()
	if err != nil {
		return false, err
	}
	return compareMarkerValues(left, leftIsVariable, operator, right, rightIsVariable)
}

// parseValue 返回字面值与「是否来自版本变量」；变量在此处已替换为环境里的取值。
func (p *markerParser) parseValue() (string, bool, error) {
	token, ok := p.peek()
	if !ok {
		return "", false, fmt.Errorf("%w: missing operand", errMarkerSyntax)
	}
	p.position++
	switch token.kind {
	case markerTokenString:
		return token.value, false, nil
	case markerTokenIdentifier:
		value, known := p.env.values[token.value]
		if !known {
			return "", false, fmt.Errorf("%w: unknown variable %q", errMarkerSyntax, token.value)
		}
		_, isVersion := versionVariables[token.value]
		return value, isVersion, nil
	default:
		return "", false, fmt.Errorf("%w: unexpected token", errMarkerSyntax)
	}
}

func (p *markerParser) parseComparisonOperator() (string, error) {
	token, ok := p.peek()
	if !ok || token.kind != markerTokenOperator {
		return "", fmt.Errorf("%w: missing operator", errMarkerSyntax)
	}
	p.position++
	switch token.value {
	case "==", "!=", "<", "<=", ">", ">=", "===", "in":
		return token.value, nil
	case "not":
		if !p.acceptOperator("in") {
			return "", fmt.Errorf("%w: expected in after not", errMarkerSyntax)
		}
		return "not in", nil
	default:
		return "", fmt.Errorf("%w: unexpected operator %q", errMarkerSyntax, token.value)
	}
}

func compareMarkerValues(left string, leftIsVersion bool, operator, right string, rightIsVersion bool) (bool, error) {
	switch operator {
	case "in":
		return strings.Contains(right, left), nil
	case "not in":
		return !strings.Contains(right, left), nil
	case "===":
		return left == right, nil
	}
	if leftIsVersion || rightIsVersion {
		comparison, err := compareSimpleVersions(left, right)
		if err != nil {
			return false, err
		}
		return applyOrdering(operator, comparison), nil
	}
	comparison := strings.Compare(left, right)
	return applyOrdering(operator, comparison), nil
}

func applyOrdering(operator string, comparison int) bool {
	switch operator {
	case "==":
		return comparison == 0
	case "!=":
		return comparison != 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	case ">":
		return comparison > 0
	default:
		return comparison >= 0
	}
}

// compareSimpleVersions 按数字段逐段比较（3.9 < 3.10），缺失段视为 0；非数字段视为语法错误。
func compareSimpleVersions(left, right string) (int, error) {
	leftParts := strings.Split(strings.TrimSpace(left), ".")
	rightParts := strings.Split(strings.TrimSpace(right), ".")
	length := max(len(leftParts), len(rightParts))
	for index := 0; index < length; index++ {
		leftValue, err := versionSegment(leftParts, index)
		if err != nil {
			return 0, err
		}
		rightValue, err := versionSegment(rightParts, index)
		if err != nil {
			return 0, err
		}
		if leftValue != rightValue {
			if leftValue < rightValue {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func versionSegment(parts []string, index int) (int, error) {
	if index >= len(parts) {
		return 0, nil
	}
	value, err := strconv.Atoi(parts[index])
	if err != nil {
		return 0, fmt.Errorf("%w: version segment %q", errMarkerSyntax, parts[index])
	}
	return value, nil
}
