package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// newRoot 按架构文档命令树构建根命令，并注册全部全局参数与骨架命令。
// 每次调用返回一棵全新的树：预解析与正式执行各用一棵，不共享任何 flag 状态
// （设计 D2「无跨调用共享状态」）。
func newRoot(deps *deps) *cobra.Command {
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
		newM5RepairCommand(deps, "repair", "重验 uv、重装受管 Python，并重建 venv 与锁定依赖"),
		cleanupCommand(deps),
	)
	// SetHelpCommand 只写 Command.helpCommand 字段，真正把它挂进子命令列表的是
	// InitDefaultHelpCmd()，而 Cobra 只在 ExecuteC() 内部调用后者。Execute 的
	// 预解析走的是 Find()，不经过 ExecuteC，因此必须在这里显式建立：否则预解析树
	// 里根本没有 help，`help doctor` 会被 legacyArgs 判成未知命令（T3.7 F1）。
	// 该调用同时保证预解析树与正式执行树的命令集合一致；它内部先 RemoveCommand
	// 再 AddCommand，与 ExecuteC 的二次调用幂等。
	root.InitDefaultHelpCmd()
	localizeHelpFlags(root)
	return root
}

// localizeHelpFlags 把 Cobra 自动生成的 -h/--help 用法文案改成中文。
// Cobra 的默认值是 "help for <command>"，与全中文的 Short/Long 混排；
// 该 flag 在每个命令上按需惰性创建，因此逐个命令显式建立后覆写。
func localizeHelpFlags(cmd *cobra.Command) {
	cmd.InitDefaultHelpFlag()
	if flag := cmd.Flags().Lookup("help"); flag != nil {
		flag.Usage = "显示 " + cmd.CommandPath() + " 的帮助"
	}
	for _, child := range cmd.Commands() {
		localizeHelpFlags(child)
	}
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
		// 叶子命令不接受位置参数，多余参数走参数错误路径（stderr + 退出码 2）。
		Args: cobra.NoArgs,
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

func workspaceGroup(deps *deps) *cobra.Command {
	group := &cobra.Command{Use: "workspace", Short: "受管后端仓库操作"}
	group.AddCommand(
		workspaceCheckCommand(deps),
		workspaceSyncCommand(deps),
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
