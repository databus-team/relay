package main

import (
	"fmt"
	"os"
	"strings"
	"text/template"
	"unicode/utf8"

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
		Title: "Core Commands:",
		Names: []string{"run", "start", "restart", "ps", "logs", "stop"},
	},
	{
		Title: "Cluster & Network:",
		Names: []string{"nodes", "tunnel"},
	},
	{
		Title: "Workspace & Sync:",
		Names: []string{"push", "pull", "diff", "undo", "ws", "sync"},
	},
	{
		Title: "Daemon:",
		Names: []string{"node"},
	},
	{
		Title: "Utility:",
		Names: []string{"version", "server-remote", "completion", "job run"},
	},
	{
		Title: "Legacy / Archive:",
		Names: []string{"server", "watch", "exec", "list", "cleanup"},
	},
}

// Style 控制终端色彩与文本加粗
type Style struct {
	enabled bool
}

func newStyle() *Style {
	return &Style{enabled: isColorSupported()}
}

func isColorSupported() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func (s *Style) Bold(str string) string {
	if !s.enabled || str == "" {
		return str
	}
	return "\033[1m" + str + "\033[0m"
}

func (s *Style) CyanBold(str string) string {
	if !s.enabled || str == "" {
		return str
	}
	return "\033[1;36m" + str + "\033[0m"
}

func (s *Style) GreenBold(str string) string {
	if !s.enabled || str == "" {
		return str
	}
	return "\033[1;32m" + str + "\033[0m"
}

func (s *Style) Yellow(str string) string {
	if !s.enabled || str == "" {
		return str
	}
	return "\033[33m" + str + "\033[0m"
}

func (s *Style) Dim(str string) string {
	if !s.enabled || str == "" {
		return str
	}
	return "\033[90m" + str + "\033[0m"
}

// displayWidth 计算字符串在等宽终端下的可视显示宽度（CJK 字符计 2，半角字符计 1）
func displayWidth(s string) int {
	w := 0
	inEsc := false
	for i := 0; i < len(s); i++ {
		// 跳过 ANSI escape sequence
		if s[i] == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if s[i] == 'm' {
				inEsc = false
			}
			continue
		}
		r := rune(s[i])
		if r > 127 {
			// 解码多字节 UTF-8
			_, size := utf8.DecodeRuneInString(s[i:])
			if size > 1 {
				i += size - 1
			}
			w += 2
		} else {
			w += 1
		}
	}
	return w
}

// padRight 以可视宽度向右补齐空格
func padRight(s string, targetWidth int) string {
	w := displayWidth(s)
	if w >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-w)
}

// decorateDesc 为帮助文本中的补充说明（如括号内容）添加次级灰度样式
func decorateDesc(style *Style, text string) string {
	if !style.enabled {
		return text
	}
	// 将 (xxx) 或 [xxx] 渲染为 Dim
	var out strings.Builder
	inParen := false
	for _, r := range text {
		if r == '(' || r == '[' {
			inParen = true
			out.WriteString("\033[90m")
			out.WriteRune(r)
		} else if r == ')' || r == ']' {
			out.WriteRune(r)
			out.WriteString("\033[0m")
			inParen = false
		} else {
			out.WriteRune(r)
		}
	}
	if inParen {
		out.WriteString("\033[0m")
	}
	return out.String()
}

// formatFlagLabel 生成整洁规整的标志标签（如 `-c, --config <path>`）
func formatFlagLabel(f *kingpin.FlagModel) string {
	var s string
	if f.Short != 0 {
		s = fmt.Sprintf("-%c, --%s", f.Short, f.Name)
	} else {
		s = fmt.Sprintf("    --%s", f.Name)
	}
	if !f.IsBoolFlag() {
		ph := f.PlaceHolder
		if ph == "" {
			switch f.Name {
			case "config":
				ph = "<path>"
			case "watch":
				ph = "<workspace>"
			case "executor", "node":
				ph = "<node-id>"
			default:
				ph = "<value>"
			}
		} else if !strings.HasPrefix(ph, "<") && !strings.HasPrefix(ph, "[") {
			ph = "<" + ph + ">"
		}
		s += " " + ph
	}
	return s
}

// formatFlagDesc 提取选项说明并附带默认值
func formatFlagDesc(f *kingpin.FlagModel) string {
	if f.Name == "help" {
		return "Show context-sensitive help"
	}
	desc := f.Help
	if len(f.Default) > 0 && f.Default[0] != "" && !f.IsBoolFlag() && f.Default[0] != "false" {
		desc += fmt.Sprintf(" (default: %s)", f.Default[0])
	}
	return desc
}

// renderHelp 生成现代化高颜值 CLI Help 文本
func renderHelp(app *kingpin.ApplicationModel, selCmd *kingpin.CmdModel, flags []*kingpin.FlagModel, args []*kingpin.ArgModel) string {
	style := newStyle()
	var b strings.Builder

	// 1. 顶级 --help 视图
	if selCmd == nil {
		b.WriteString(style.CyanBold("Relay") + " - Peer-to-peer execution and workspace synchronization\n\n")

		b.WriteString(style.CyanBold("Usage:") + "\n")
		b.WriteString("  " + style.Bold("relay") + " [flags] <command> [args...]\n\n")

		// 全局 Flags
		if len(flags) > 0 {
			var validFlags []*kingpin.FlagModel
			var helpFlag *kingpin.FlagModel
			for _, f := range flags {
				if !f.Hidden {
					if f.Name == "help" {
						helpFlag = f
					} else {
						validFlags = append(validFlags, f)
					}
				}
			}
			if helpFlag != nil {
				validFlags = append(validFlags, helpFlag)
			}
			if len(validFlags) > 0 {
				b.WriteString(style.CyanBold("Flags:") + "\n")
				maxLabelWidth := 0
				labels := make([]string, len(validFlags))
				for i, f := range validFlags {
					labels[i] = formatFlagLabel(f)
					if w := displayWidth(labels[i]); w > maxLabelWidth {
						maxLabelWidth = w
					}
				}
				if maxLabelWidth < 22 {
					maxLabelWidth = 22
				}
				for i, f := range validFlags {
					b.WriteString("  " + style.GreenBold(padRight(labels[i], maxLabelWidth+2)) + decorateDesc(style, formatFlagDesc(f)) + "\n")
				}
				b.WriteString("\n")
			}
		}

		// 分组 Commands (既支持顶级命令如 tunnel，也支持扁平子命令如 job run)
		cmdMap := make(map[string]*kingpin.CmdModel)
		for _, cmd := range app.Commands {
			if !cmd.Hidden {
				cmdMap[cmd.Name] = cmd
			}
		}
		for _, cmd := range app.FlattenedCommands() {
			if !cmd.Hidden {
				cmdMap[cmd.FullCommand] = cmd
			}
		}

		rendered := make(map[string]bool)
		for _, cat := range categories {
			var catCmds []*kingpin.CmdModel
			for _, name := range cat.Names {
				if cmd, ok := cmdMap[name]; ok {
					catCmds = append(catCmds, cmd)
					rendered[name] = true
				}
			}
			if len(catCmds) == 0 {
				continue
			}

			b.WriteString(style.CyanBold(cat.Title) + "\n")
			maxCmdWidth := 16
			for _, cmd := range catCmds {
				dispName := cmd.FullCommand
				if dispName == "" {
					dispName = cmd.Name
				}
				if w := displayWidth(dispName); w > maxCmdWidth {
					maxCmdWidth = w
				}
			}
			for _, cmd := range catCmds {
				dispName := cmd.FullCommand
				if dispName == "" {
					dispName = cmd.Name
				}
				label := padRight(dispName, maxCmdWidth+2)
				b.WriteString("  " + style.GreenBold(label) + decorateDesc(style, cmd.Help) + "\n")
			}
			b.WriteString("\n")
		}

		// 检查兜底未分组命令
		var otherCmds []*kingpin.CmdModel
		for _, cmd := range app.FlattenedCommands() {
			top := strings.Split(cmd.FullCommand, " ")[0]
			if !cmd.Hidden && !rendered[cmd.FullCommand] && !rendered[top] && cmd.FullCommand != "help" {
				otherCmds = append(otherCmds, cmd)
			}
		}
		if len(otherCmds) > 0 {
			b.WriteString(style.CyanBold("Other Commands:") + "\n")
			for _, cmd := range otherCmds {
				label := padRight(cmd.FullCommand, 18)
				b.WriteString("  " + style.GreenBold(label) + decorateDesc(style, cmd.Help) + "\n")
			}
			b.WriteString("\n")
		}

		b.WriteString(style.Dim("Use \"relay <command> --help\" for more information about a command.") + "\n")
		return b.String()
	}

	// 2. 具体子命令 --help 视图
	b.WriteString(style.CyanBold("Usage:") + "\n")
	usageLine := "  " + style.Bold("relay "+selCmd.FullCommand)
	if len(flags) > 0 {
		usageLine += " [flags]"
	}
	if len(args) > 0 {
		for _, arg := range args {
			if !arg.Hidden {
				ph := arg.PlaceHolder
				if ph == "" {
					ph = "<" + arg.Name + ">"
				}
				if !arg.Required {
					ph = "[" + ph + "]"
				}
				usageLine += " " + ph
			}
		}
	}
	b.WriteString(usageLine + "\n\n")

	if selCmd.Help != "" {
		b.WriteString(decorateDesc(style, selCmd.Help) + "\n\n")
	}

	// 子命令如果还有下级子命令
	if len(selCmd.Commands) > 0 {
		var subCmds []*kingpin.CmdModel
		for _, sc := range selCmd.FlattenedCommands() {
			if !sc.Hidden && sc.FullCommand != selCmd.FullCommand {
				subCmds = append(subCmds, sc)
			}
		}
		if len(subCmds) > 0 {
			b.WriteString(style.CyanBold("Available Subcommands:") + "\n")
			maxSubWidth := 14
			for _, sc := range subCmds {
				name := strings.TrimPrefix(sc.FullCommand, selCmd.FullCommand+" ")
				if w := displayWidth(name); w > maxSubWidth {
					maxSubWidth = w
				}
			}
			for _, sc := range subCmds {
				name := strings.TrimPrefix(sc.FullCommand, selCmd.FullCommand+" ")
				label := padRight(name, maxSubWidth+2)
				b.WriteString("  " + style.GreenBold(label) + decorateDesc(style, sc.Help) + "\n")
			}
			b.WriteString("\n")
		}
	}

	// 标志 Flags
	if len(flags) > 0 {
		var validFlags []*kingpin.FlagModel
		var helpFlag *kingpin.FlagModel
		for _, f := range flags {
			if !f.Hidden {
				if f.Name == "help" {
					helpFlag = f
				} else {
					validFlags = append(validFlags, f)
				}
			}
		}
		if helpFlag != nil {
			validFlags = append(validFlags, helpFlag)
		}
		if len(validFlags) > 0 {
			b.WriteString(style.CyanBold("Flags:") + "\n")
			maxLabelWidth := 0
			labels := make([]string, len(validFlags))
			for i, f := range validFlags {
				labels[i] = formatFlagLabel(f)
				if w := displayWidth(labels[i]); w > maxLabelWidth {
					maxLabelWidth = w
				}
			}
			if maxLabelWidth < 20 {
				maxLabelWidth = 20
			}
			for i, f := range validFlags {
				b.WriteString("  " + style.GreenBold(padRight(labels[i], maxLabelWidth+2)) + decorateDesc(style, formatFlagDesc(f)) + "\n")
			}
			b.WriteString("\n")
		}
	}

	// 参数 Arguments
	if len(args) > 0 {
		var validArgs []*kingpin.ArgModel
		for _, a := range args {
			if !a.Hidden {
				validArgs = append(validArgs, a)
			}
		}
		if len(validArgs) > 0 {
			b.WriteString(style.CyanBold("Arguments:") + "\n")
			maxArgWidth := 0
			names := make([]string, len(validArgs))
			for i, a := range validArgs {
				ph := a.PlaceHolder
				if ph == "" {
					ph = "<" + a.Name + ">"
				}
				names[i] = ph
				if w := displayWidth(names[i]); w > maxArgWidth {
					maxArgWidth = w
				}
			}
			if maxArgWidth < 14 {
				maxArgWidth = 14
			}
			for i, a := range validArgs {
				b.WriteString("  " + style.GreenBold(padRight(names[i], maxArgWidth+2)) + decorateDesc(style, a.Help) + "\n")
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

func setupUsageTemplate() {
	kingpin.CommandLine.UsageFuncs(template.FuncMap{
		"RenderHelp": func(app *kingpin.ApplicationModel, selCmd *kingpin.CmdModel, flags []*kingpin.FlagModel, args []*kingpin.ArgModel) string {
			return renderHelp(app, selCmd, flags, args)
		},
	})
	kingpin.CommandLine.UsageTemplate("{{RenderHelp .App .Context.SelectedCommand .Context.Flags .Context.Args}}")
}
