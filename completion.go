package task

import (
	_ "embed"
	"fmt"
)

//go:embed completion/bash/taskgraph.bash
var completionBash string

//go:embed completion/fish/taskgraph.fish
var completionFish string

//go:embed completion/ps/taskgraph.ps1
var completionPowershell string

//go:embed completion/zsh/_taskgraph
var completionZsh string

func Completion(completion string) (string, error) {
	// Get the file extension for the selected shell
	switch completion {
	case "bash":
		return completionBash, nil
	case "fish":
		return completionFish, nil
	case "powershell":
		return completionPowershell, nil
	case "zsh":
		return completionZsh, nil
	default:
		return "", fmt.Errorf("unknown shell: %s", completion)
	}
}
