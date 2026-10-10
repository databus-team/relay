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
    local cur prev opts base
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    opts=$( ${COMP_WORDS[0]} --completion-bash "${COMP_WORDS[@]:1:$COMP_CWORD}" )
    COMPREPLY=( $(compgen -W "${opts}" -- ${cur}) )
    return 0
}

complete -F _relay_bash_autocomplete -o default relay
`

const zshCompletionScript = `#compdef relay
# zsh completion for relay

_relay() {
    local -a matches
    matches=($(${words[1]} --completion-bash "${(@)words[2,$CURRENT]}"))
    compadd -a matches

    if [[ $compstate[nmatches] -eq 0 && $words[$CURRENT] != -* ]]; then
        _files
    fi
}

if [[ "$(basename -- ${(%):-%x})" != "_relay" ]]; then
    compdef _relay relay
fi
`

const fishCompletionScript = `# fish completion for relay

function __fish_relay_complete
    set -l cmd (commandline -co)
    set -e cmd[1]
    relay --completion-bash $cmd
end

complete -c relay -f -a "(__fish_relay_complete)"
`

const powershellCompletionScript = `# PowerShell completion for relay

Register-ArgumentCompleter -Native -CommandName relay -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = $commandAst.ToString().Split(' ') | Where-Object { $_ }
    $completions = & relay --completion-bash $words[1..($words.Length-1)]
    $completions | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`

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

// getWorkspaceHints 从本地配置文件动态提取所有工作区 ID，供 shell 自动补全
func getWorkspaceHints() []string {
	cfg, err := config.Load(*configPath)
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
	cfg, err := config.Load(*configPath)
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
