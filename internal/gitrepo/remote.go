package gitrepo

import (
	"context"
	"errors"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// CheckRemote 只读取当前仓库和远端分支引用，不创建受管状态或下载工作区。
func (s *Service) CheckRemote(ctx context.Context, policy mirror.Policy) (RemoteCheckResult, error) {
	current, err := s.Check(ctx)
	if err != nil {
		return RemoteCheckResult{}, err
	}
	if !current.Healthy {
		return RemoteCheckResult{Current: current}, nil
	}
	target, err := ParseTarget(current.Version)
	if err != nil {
		return RemoteCheckResult{}, serviceCheckReadError(err)
	}
	plan, err := s.buildPlan(ctx, policy)
	if err != nil {
		return RemoteCheckResult{}, servicePolicyArgumentError(err)
	}
	commit, err := s.resolveRemote(ctx, plan, target)
	if err != nil {
		return RemoteCheckResult{}, err
	}
	return RemoteCheckResult{Current: current, RemoteCommit: commit, UpdateAvailable: commit != current.Commit}, nil
}

// resolveRemoteCommit 将网络预算与镜像轮换绑定，检查失败不产生本地副作用。
func resolveRemoteCommit(ctx context.Context, plan mirror.Plan, target Target) (string, error) {
	return resolveRemoteWithClient(ctx, plan, target, goGitClient{})
}

func resolveRemoteWithClient(ctx context.Context, plan mirror.Plan, target Target, client gitClient, options ...mirror.RotationOption) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	rotator, err := mirror.NewRotator(options...)
	if err != nil {
		return "", serviceInternalError(protocol.StageWorkspaceCheck, err)
	}
	mt, err := mirror.NewTarget(mirror.TargetSpec{ProductVersion: target.Version(), ReleaseBranch: target.Branch()})
	if err != nil {
		return "", serviceInternalError(protocol.StageWorkspaceCheck, err)
	}
	var httpStatuses []int
	result, err := rotator.Run(ctx, plan, mt, func(attemptCtx context.Context, attempt mirror.Attempt) mirror.AttemptOutcome {
		httpStatuses = append(httpStatuses, 0)
		refs, err := client.ListReferences(attemptCtx, attempt.Source.BaseURL(), nil)
		if err != nil {
			kind, status := classifyRemoteFailure(err)
			httpStatuses[len(httpStatuses)-1] = status
			return failedOutcome(mirror.OutcomeSwitchSource, kind, newError(protocol.CodeGitRemoteResolveFailed, protocol.StageWorkspaceCheck, messageForCode(protocol.CodeGitRemoteResolveFailed), nil, err))
		}
		for _, ref := range refs {
			if ref != nil && ref.Name() == plumbing.NewBranchReferenceName(target.Branch()) && validCommit(ref.Hash().String()) {
				return mirror.AttemptOutcome{Kind: mirror.OutcomeSucceeded, ActualCommit: ref.Hash().String()}
			}
		}
		return failedOutcome(mirror.OutcomeSwitchSource, failureBranchMissing, newError(protocol.CodeGitBranchNotFound, protocol.StageWorkspaceCheck, messageForCode(protocol.CodeGitBranchNotFound), nil, errors.New("release branch is absent")))
	})
	if err != nil {
		failure := mapFetchFailure(ctx, err, result.Reports)
		// 复用汇总优先级，但只读引用查询没有进入克隆阶段。
		failure.stage = protocol.StageWorkspaceCheck
		attempts := failure.details["attempts"].([]map[string]any)
		for i, status := range httpStatuses {
			if i < len(attempts) && status != 0 {
				attempts[i]["httpStatus"] = status
			}
		}
		return "", failure
	}
	return result.ActualCommit, nil
}
