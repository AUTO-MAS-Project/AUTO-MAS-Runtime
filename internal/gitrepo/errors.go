package gitrepo

import (
	"fmt"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// Error 描述工作区 Git 服务失败，供 CLI 映射协议四元组。
type Error struct {
	code    protocol.Code
	stage   protocol.Stage
	message string
	details map[string]any
	cause   error
}

func newError(
	code protocol.Code,
	stage protocol.Stage,
	message string,
	details map[string]any,
	cause error,
) *Error {
	return &Error{
		code:    code,
		stage:   stage,
		message: message,
		details: details,
		cause:   cause,
	}
}

func (e *Error) Error() string {
	if e == nil {
		return "git repository operation failed"
	}
	if e.cause == nil {
		return e.message
	}
	return fmt.Sprintf("%s: %v", e.message, e.cause)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Code 返回稳定协议错误码。
func (e *Error) Code() protocol.Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Stage 返回失败发生的稳定阶段。
func (e *Error) Stage() protocol.Stage {
	if e == nil {
		return ""
	}
	return e.stage
}

// Message 返回不包含路径、URL 或底层错误文本的用户文案。
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Details 返回只包含稳定分类事实的协议详情。
func (e *Error) Details() map[string]any {
	if e == nil {
		return nil
	}
	return e.details
}

func messageForCode(code protocol.Code) string {
	switch code {
	case protocol.CodeInvalidVersion:
		return "目标版本无效"
	case protocol.CodeOperationCancelled:
		return "仓库同步已取消"
	case protocol.CodeOutputWriteFailed:
		return "无法写入仓库同步进度"
	case protocol.CodeNetworkUnavailable:
		return "当前策略禁止访问 Git 网络源"
	case protocol.CodeGitRemoteResolveFailed:
		return "无法查询目标发布分支"
	case protocol.CodeGitBranchNotFound:
		return "目标发布分支不存在"
	case protocol.CodeGitCloneFailed:
		return "无法克隆后端仓库"
	case protocol.CodeGitRepositoryInvalid:
		return "克隆的后端仓库无效"
	case protocol.CodeGitVersionMismatch:
		return "仓库版本与目标版本不一致"
	case protocol.CodeGitRepoCleanupFailed:
		return "无法清理仓库临时目录"
	case protocol.CodeUpdateStateAmbiguous:
		return "仓库更新现场无法安全判定"
	case protocol.CodeMirrorExhausted:
		return "所有 Git 网络源均不可用"
	default:
		return "仓库同步失败"
	}
}

var _ error = (*Error)(nil)
