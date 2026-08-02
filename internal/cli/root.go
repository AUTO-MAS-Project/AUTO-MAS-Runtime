package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// newRoot 按架构文档命令树构建根命令，并注册全部全局参数与骨架命令。
func newRoot(deps *deps, io IO) *cobra.Command {
	root := &cobra.Command{
		Use:   "auto-mas-runtime",
		Short: "AUTO-MAS 本机运行时管理程序",
		Long: "auto-mas-runtime 管理本机 AUTO-MAS 运行时的环境、更新与后端进程，" +
			"通过命令行参数、NDJSON 标准输出和退出码与 Electron 通信。",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			values, err := parseGlobalOptions(cmd.Flags(), deps.options.cwd)
			if err != nil {
				return err
			}
			deps.global = values
			return nil
		},
	}
	root.PersistentFlags().String("app-root", ".", "应用根目录（默认当前工作目录）")
	root.PersistentFlags().String("output", "human", "输出模式：human 或 ndjson")
	root.PersistentFlags().String("protocol", strconv.Itoa(protocol.Version), "Runtime 协议版本")
	root.PersistentFlags().StringArray("mirror", nil, "镜像源 <kind>=<key>，可重复指定")
	root.PersistentFlags().Bool("offline", false, "禁止任何网络访问")
	root.PersistentFlags().Bool("mirror-only", false, "只使用配置的镜像源，排除官方源")
	root.CompletionOptions.DisableDefaultCmd = true
	// 帮助模板过滤隐藏的 help 命令，使清单与架构文档命令树严格一致。
	root.SetUsageTemplate(usageTemplate)
	root.SetHelpCommand(&cobra.Command{
		Use:    "help",
		Short:  "显示命令帮助",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := cmd.Root().Find(args)
			if err != nil {
				return err
			}
			return target.Help()
		},
	})
	root.AddCommand(
		versionCommand(deps),
		doctorCommand(deps),
		bootstrapCommand(deps),
		workspaceGroup(deps),
		environmentGroup(deps),
		dependenciesGroup(deps),
		backendGroup(deps),
		skeletonCommand(deps, "repair", "修复运行环境", protocol.StageRepair),
		skeletonCommand(deps, "cleanup", "清理可丢弃缓存与临时内容", protocol.StageCleanup),
	)
	return root
}

// usageTemplate 与 Cobra 默认模板一致，仅把 Available Commands 过滤条件
// 收紧为 IsAvailableCommand，避免隐藏的 help 命令出现在命令清单中。
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// skeletonCommand 注册已定界面但尚未实现的命令。
// 所有骨架命令统一返回 UNSUPPORTED_MODE，并在 hello 后以 error+result 收口。
func skeletonCommand(deps *deps, use, short string, stage protocol.Stage) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperation(
				deps.ctx,
				deps,
				commandPath(cmd),
				stage,
				func(context.Context, *protocol.Emitter) (sessionSuccess, error) {
					return sessionSuccess{}, notImplementedError{stage: stage}
				},
			)
			return nil
		},
	}
}

func bootstrapCommand(deps *deps) *cobra.Command {
	command := skeletonCommand(deps, "bootstrap", "准备受管后端环境", protocol.StageBootstrap)
	command.Flags().String("version", "", "目标版本（例如 v5.4.0-beta.1）")
	return command
}

func workspaceGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "workspace", Short: "受管后端仓库操作"}
	group.AddCommand(
		skeletonCommand(deps, "check", "只读检查受管仓库", protocol.StageWorkspaceCheck),
		workspaceSyncCommand(deps),
	)
	return group
}

func workspaceSyncCommand(deps *deps) *cobra.Command {
	command := skeletonCommand(deps, "sync", "同步受管仓库到目标版本", protocol.StageWorkspaceSwap)
	command.Flags().String("version", "", "目标版本（例如 v5.4.0-beta.1）")
	return command
}

func environmentGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "environment", Short: "运行环境操作"}
	group.AddCommand(
		skeletonCommand(deps, "check", "只读检查 uv 与 Python 环境", protocol.StageUVCheck),
		skeletonCommand(deps, "ensure", "准备固定版本 uv", protocol.StageUVCheck),
		skeletonCommand(deps, "repair", "重新下载并校验 uv 与受管 Python", protocol.StageUVVerify),
	)
	return group
}

func dependenciesGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "dependencies", Short: "主项目依赖操作"}
	group.AddCommand(
		skeletonCommand(deps, "check", "只读检查依赖环境", protocol.StageDependenciesCheck),
		skeletonCommand(deps, "sync", "按锁文件同步主项目依赖", protocol.StageDependenciesSync),
		skeletonCommand(deps, "rebuild", "重建主项目环境", protocol.StageDependenciesRebuild),
	)
	return group
}

func backendGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "backend", Short: "后端进程操作"}
	group.AddCommand(
		skeletonCommand(deps, "supervise", "启动并监督后端进程", protocol.StageBackendSpawn),
	)
	return group
}

// commandPath 返回 hello.command 使用的命令路径，去掉根命令名前缀。
func commandPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	rootName := cmd.Root().Name()
	if rootName != "" && strings.HasPrefix(path, rootName+" ") {
		return strings.TrimPrefix(path, rootName+" ")
	}
	return path
}
