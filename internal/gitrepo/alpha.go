package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"
)

const alphaConfigSection = "auto-mas-runtime"

type alphaBinding struct {
	version       string
	commit        string
	sourceVersion string
}

func readAlphaBinding(raw *gitconfig.Config) (*alphaBinding, error) {
	var binding *alphaBinding
	for _, section := range raw.Sections {
		if !section.IsName(alphaConfigSection) {
			continue
		}
		if binding != nil || len(section.Subsections) != 0 || len(section.Options) != 3 {
			return nil, errInvalidRepositoryID
		}
		values := make(map[string]string, 3)
		for _, option := range section.Options {
			key := strings.ToLower(option.Key)
			if key != "version" && key != "commit" && key != "sourceversion" {
				return nil, errInvalidRepositoryID
			}
			if _, exists := values[key]; exists {
				return nil, errInvalidRepositoryID
			}
			values[key] = option.Value
		}
		binding = &alphaBinding{version: values["version"], commit: values["commit"], sourceVersion: values["sourceversion"]}
		target, err := ParseTarget(binding.version)
		if err != nil || target.Branch() != "dev" || !validCommit(binding.commit) || !validVersion(binding.sourceVersion) {
			return nil, errInvalidRepositoryID
		}
	}
	return binding, nil
}

// bindAlphaRevision 只在完整校验后持目录租约写入身份，不改任何受管源码。
// 绑定随目录替换，避免崩溃恢复依赖当前远端 dev 或猜测构建版本。
func bindAlphaRevision(ctx context.Context, access repositoryAccess, revision Revision) (returnErr error) {
	if revision.Branch() != "dev" {
		return nil
	}
	repository, files, err := openGitRepository(ctx, access)
	if err != nil {
		return fmt.Errorf("open alpha repository: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, files.close()) }()
	snapshot, err := inspectOpenRepository(ctx, repository)
	if err != nil {
		return err
	}
	sourceVersion, err := parseRepositoryVersion(snapshot.versionPayload)
	if err != nil || !validVersion(sourceVersion) || snapshot.commit != revision.Commit() || snapshot.headTarget != "refs/heads/dev" {
		return errors.Join(errInvalidRepositoryID, err)
	}

	cfg, err := repository.Config()
	if err != nil {
		return fmt.Errorf("read alpha config: %w", err)
	}
	section := cfg.Raw.Section(alphaConfigSection)
	section.SetOption("version", revision.Version())
	section.SetOption("commit", revision.Commit())
	section.SetOption("sourceversion", sourceVersion)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := repository.SetConfig(cfg); err != nil {
		return fmt.Errorf("write alpha identity: %w", err)
	}
	snapshot, err = inspectOpenRepository(ctx, repository)
	if err != nil {
		return err
	}
	if snapshot.alphaBinding == nil || snapshot.alphaBinding.version != revision.Version() ||
		snapshot.alphaBinding.commit != snapshot.commit || snapshot.commit != revision.Commit() ||
		snapshot.alphaBinding.sourceVersion != sourceVersion {
		return errInvalidRepositoryID
	}
	return ctx.Err()
}
