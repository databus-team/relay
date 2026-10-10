package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/config"
)

var (
	completionCmd   = kingpin.Command("completion", "Generate shell completion script (bash, zsh, fish, powershell)")
	completionShell = completionCmd.Arg("shell", "Target shell (bash, zsh, fish, powershell)").Required().HintOptions("bash", "zsh", "fish", "powershell").String()
)

const bashCompletionScript = `# bash completion for relay -*- shell-script -*-

_relay_bash_autocomplete() {
    local cur prev words cword
    _get_comp_words_by_ref -n =: cur prev words cword 2>/dev/null || {
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    }

    local IFS=$'\n'
    COMPREPLY=($( ${COMP_WORDS[0]} __complete "${words[@]:1:$cword}" ))
    return 0
}

complete -F _relay_bash_autocomplete -o default -o nospace relay
`

const zshCompletionScript = `#compdef relay
# zsh completion for relay

# Ensure compinit is loaded safely
if ! type compdef >/dev/null 2>&1; then
    autoload -Uz compinit && compinit -C 2>/dev/null || compinit 2>/dev/null
fi

_relay() {
    local -a matches
    local IFS=$'\n'

    # Request suggestions from relay __complete
    matches=($(${words[1]} __complete "${(@)words[2,$CURRENT]}"))

    if [[ ${#matches[@]} -gt 0 ]]; then
        compadd -Q -a matches
    else
        _files
    fi
}

compdef _relay relay
`

const fishCompletionScript = `# fish completion for relay

function __fish_relay_complete
    set -l cmd (commandline -co)
    set -e cmd[1]
    relay __complete $cmd
end

complete -c relay -f -a "(__fish_relay_complete)"
`

const powershellCompletionScript = `# PowerShell completion for relay

Register-ArgumentCompleter -Native -CommandName relay -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = $commandAst.ToString().Split(' ') | Where-Object { $_ }
    $completions = & relay __complete $words[1..($words.Length-1)]
    $completions | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`

type cmdSpec struct {
	subcmds []string
	flags   []string
}

var topCommands = []string{
	"run", "ps", "logs", "stop",
	"nodes", "tunnel",
	"push", "pull", "diff", "undo", "ws", "sync",
	"node", "version", "server-remote", "completion", "job",
	"server", "watch", "list", "cleanup",
}

var globalFlags = []string{
	"-c", "--config", "--debug", "-h", "--help",
}

var commandSpecs = map[string]cmdSpec{
	"run": {
		flags: []string{"-w", "--watch", "-e", "--executor", "--node", "-f", "--force-remote", "--no-sync", "-d", "--detach", "-p", "--publish", "--bind-host"},
	},
	"nodes": {
		flags: []string{"--json", "-u", "--usage"},
	},
	"ps": {
		flags: []string{"-a", "--all", "-e", "--executor", "--node", "--json"},
	},
	"logs": {
		flags: []string{"-f", "--follow", "--tail", "-e", "--executor", "--node"},
	},
	"stop": {
		flags: []string{"-e", "--executor", "--node"},
	},
	"push": {
		flags: []string{"-w", "--watch", "-e", "--executor", "--node", "--dest", "--no-jobs"},
	},
	"pull": {
		flags: []string{"-w", "--watch", "-d", "--delete"},
	},
	"diff": {
		flags: []string{"-w", "--watch", "-e", "--executor", "--node"},
	},
	"undo": {
		flags: []string{"-w", "--watch", "-e", "--executor", "--node", "-l", "--list", "--snapshot"},
	},
	"ws": {
		flags: []string{"--name", "--json", "-v", "--verbose"},
	},
	"sync": {
		flags: []string{"-e", "--executor", "--node"},
	},
	"node": {
		subcmds: []string{"run", "start", "stop", "status", "restart", "upgrade"},
	},
	"completion": {
		subcmds: []string{"bash", "zsh", "fish", "powershell"},
	},
	"job": {
		subcmds: []string{"run"},
		flags:   []string{"-w", "--watch"},
	},
	"tunnel": {
		subcmds: []string{"run", "start", "stop", "status", "logs", "restart", "upgrade"},
		flags:   []string{"--listen", "-w", "--executor", "--node", "--json", "-f", "--follow", "--tail"},
	},
	"server": {
		subcmds: []string{"run", "start", "stop", "status", "restart", "upgrade"},
		flags:   []string{"--server-config", "--addr"},
	},
	"watch": {
		subcmds: []string{"run", "start", "stop", "status", "restart", "upgrade"},
	},
	"list": {
		flags: []string{"-w", "--watch"},
	},
	"cleanup": {
		flags: []string{"-w", "--watch"},
	},
	"version": {
		flags: []string{"--json"},
	},
	"server-remote": {
		flags: []string{"--binary", "--expect"},
	},
}

func isWorkspaceFlag(s string) bool {
	return s == "-w" || s == "--watch" || s == "--name"
}

func isNodeFlag(s string) bool {
	return s == "-e" || s == "--executor" || s == "--node"
}

// handleComplete 快速高效处理 shell 发起的补全请求，秒级返回候选项
func handleComplete(args []string) {
	if len(args) == 0 {
		printSuggestions(topCommands, "")
		return
	}

	lastWord := args[len(args)-1]
	prevWord := ""
	if len(args) >= 2 {
		prevWord = args[len(args)-2]
	}

	// 1. 若前一个参数需要特定值（工作区或节点）
	if isWorkspaceFlag(prevWord) {
		printSuggestions(getWorkspaceHints(), lastWord)
		return
	}
	if isNodeFlag(prevWord) {
		printSuggestions(getNodeHints(), lastWord)
		return
	}
	if prevWord == "-c" || prevWord == "--config" {
		return // 交给 shell 走文件补全
	}

	// 2. 查找第一个命令名 (正确跳过带参数的全局 Flag)
	cmd := ""
	for i := 0; i < len(args)-1; i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			// 跳过带单独参数值的选项，如 -c /path 或 --config /path
			if (arg == "-c" || arg == "--config") && i+1 < len(args)-1 {
				i++
			}
			continue
		}
		cmd = arg
		break
	}

	// 若尚未输入确定的一级命令
	if cmd == "" {
		if strings.HasPrefix(lastWord, "-") {
			printSuggestions(globalFlags, lastWord)
		} else {
			all := append([]string{}, topCommands...)
			all = append(all, globalFlags...)
			printSuggestions(all, lastWord)
		}
		return
	}

	// 3. 处理已确定一级命令的后续补全
	spec, ok := commandSpecs[cmd]
	if !ok {
		return
	}

	// 如果命令含有下级子命令（如 node, completion, job）
	if len(spec.subcmds) > 0 {
		hasSubcmd := false
		for i := 0; i < len(args)-1; i++ {
			for _, s := range spec.subcmds {
				if args[i] == s {
					hasSubcmd = true
					break
				}
			}
		}
		if !hasSubcmd && !strings.HasPrefix(lastWord, "-") {
			printSuggestions(spec.subcmds, lastWord)
			return
		}
	}

	// 提示该命令支持的 Flags
	flags := append([]string{}, spec.flags...)
	flags = append(flags, globalFlags...)
	printSuggestions(flags, lastWord)
}

func printSuggestions(items []string, prefix string) {
	seen := make(map[string]bool)
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		if prefix == "" || strings.HasPrefix(item, prefix) {
			fmt.Println(item)
		}
	}
}

func runCompletion() {
	shell := strings.ToLower(strings.TrimSpace(*completionShell))
	switch shell {
	case "bash":
		os.Stdout.WriteString(bashCompletionScript)
	case "zsh":
		os.Stdout.WriteString(zshCompletionScript)
	case "fish":
		os.Stdout.WriteString(fishCompletionScript)
	case "powershell", "ps":
		os.Stdout.WriteString(powershellCompletionScript)
	default:
		fmt.Fprintf(os.Stderr, "Error: unsupported shell %q. Supported shells: bash, zsh, fish, powershell\n", shell)
		os.Exit(1)
	}
}

func getEffectiveConfigPath() string {
	for i := 1; i < len(os.Args)-1; i++ {
		if os.Args[i] == "-c" || os.Args[i] == "--config" {
			return os.Args[i+1]
		}
	}
	if configPath != nil && *configPath != "" {
		return *configPath
	}
	return "~/.relay/config.yaml"
}

// getWorkspaceHints 从本地配置文件动态提取所有工作区 ID，供 shell 自动补全
func getWorkspaceHints() []string {
	cfg, err := config.Load(getEffectiveConfigPath())
	if err != nil {
		return nil
	}
	var hints []string
	for _, ws := range cfg.Workspaces {
		if ws.ID != "" {
			hints = append(hints, ws.ID)
		}
	}
	return hints
}

// getNodeHints 从本地配置文件动态提取所有已知节点/执行方 ID，供 shell 自动补全
func getNodeHints() []string {
	cfg, err := config.Load(getEffectiveConfigPath())
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var hints []string
	for _, ws := range cfg.Workspaces {
		if ws.Executor != "" && !seen[ws.Executor] {
			seen[ws.Executor] = true
			hints = append(hints, ws.Executor)
		}
	}
	if cfg.Backend.Config != nil {
		if eid, ok := cfg.Backend.Config["executor_id"].(string); ok && eid != "" && !seen[eid] {
			seen[eid] = true
			hints = append(hints, eid)
		}
	}
	if cfg.Node != nil {
		if cfg.Node.ID != "" && !seen[cfg.Node.ID] {
			seen[cfg.Node.ID] = true
			hints = append(hints, cfg.Node.ID)
		}
		for _, n := range cfg.Node.AllowedControllers {
			if n != "*" && !seen[n] {
				seen[n] = true
				hints = append(hints, n)
			}
		}
	}
	return hints
}
