package main

import (
	"text/template"

	"github.com/alecthomas/kingpin/v2"
)

// CommandCategory 定义命令分组
type CommandCategory struct {
	Title string
	Names []string
}

// categories 定义所有命令的分组与展示顺序
var categories = []CommandCategory{
	{
		Title: "Core Commands (核心执行与任务编排):",
		Names: []string{"run", "ps", "logs", "stop"},
	},
	{
		Title: "Cluster & Network (集群节点拓扑与网络):",
		Names: []string{"nodes", "status", "port-forward", "tunnel"},
	},
	{
		Title: "Workspace & Sync (工作区代码同步与恢复):",
		Names: []string{"push", "pull", "diff", "undo", "ws", "sync"},
	},
	{
		Title: "Daemon (常驻守护进程):",
		Names: []string{"node"},
	},
	{
		Title: "Utility (工具与系统):",
		Names: []string{"version", "server-remote", "job run"},
	},
	{
		Title: "Legacy / Archive (旧版兼容命令与低频运维):",
		Names: []string{"server", "watch", "exec", "list", "cleanup"},
	},
}

var groupedUsageTemplate = `{{define "FormatCommand" -}}
{{if .FlagSummary}} {{.FlagSummary}}{{end -}}
{{range .Args}}{{if not .Hidden}} {{if not .Required}}[{{end}}{{if .PlaceHolder}}{{.PlaceHolder}}{{else}}<{{.Name}}>{{end}}{{if .Value|IsCumulative}}...{{end}}{{if not .Required}}]{{end}}{{end}}{{end -}}
{{end -}}

{{define "FormatUsage" -}}
{{template "FormatCommand" .}}{{if .Commands}} <command> [<args> ...]{{end}}
{{if .Help}}
{{.Help|Wrap 0 -}}
{{end -}}

{{end -}}

{{if .Context.SelectedCommand -}}
usage: {{.App.Name}} {{.Context.SelectedCommand}}{{template "FormatUsage" .Context.SelectedCommand}}
{{ else -}}
usage: {{.App.Name}}{{template "FormatUsage" .App}}
{{end}}
{{if .Context.Flags -}}
Flags:
{{.Context.Flags|FlagsToTwoColumns|FormatTwoColumns}}
{{end -}}
{{if .Context.Args -}}
Args:
{{.Context.Args|ArgsToTwoColumns|FormatTwoColumns}}
{{end -}}
{{if .Context.SelectedCommand -}}
{{if len .Context.SelectedCommand.Commands -}}
Subcommands:
{{range .Context.SelectedCommand.FlattenedCommands -}}
{{if not .Hidden -}}
  {{.FullCommand}}{{if .Default}}*{{end}}{{template "FormatCommand" .}}
{{.Help|Wrap 4}}
{{end -}}
{{end -}}
{{end -}}
{{else if .App.Commands -}}
{{range (GetCategories .App)}}
{{.Title}}
{{range .Cmds -}}
  {{.FullCommand}}{{if .Default}}*{{end}}{{template "FormatCommand" .}}
{{.Help|Wrap 4}}
{{end}}
{{end -}}
{{end -}}
`

type RenderedCategory struct {
	Title string
	Cmds  []*kingpin.CmdModel
}

func setupUsageTemplate() {
	kingpin.CommandLine.UsageFuncs(template.FuncMap{
		"GetCategories": func(app *kingpin.ApplicationModel) []RenderedCategory {
			cmdMap := make(map[string]*kingpin.CmdModel)
			for _, cmd := range app.FlattenedCommands() {
				if !cmd.Hidden {
					cmdMap[cmd.FullCommand] = cmd
				}
			}

			var result []RenderedCategory
			rendered := make(map[string]bool)

			for _, cat := range categories {
				var catCmds []*kingpin.CmdModel
				for _, name := range cat.Names {
					if cmd, ok := cmdMap[name]; ok {
						catCmds = append(catCmds, cmd)
						rendered[name] = true
					}
				}

				if len(catCmds) > 0 {
					result = append(result, RenderedCategory{
						Title: cat.Title,
						Cmds:  catCmds,
					})
				}
			}

			// 检查是否有未分组的命令兜底展示
			var otherCmds []*kingpin.CmdModel
			for _, cmd := range app.FlattenedCommands() {
				if !cmd.Hidden && !rendered[cmd.FullCommand] && cmd.FullCommand != "help" {
					otherCmds = append(otherCmds, cmd)
				}
			}
			if len(otherCmds) > 0 {
				result = append(result, RenderedCategory{
					Title: "Other Commands:",
					Cmds:  otherCmds,
				})
			}

			return result
		},
	})
	kingpin.CommandLine.UsageTemplate(groupedUsageTemplate)
}
