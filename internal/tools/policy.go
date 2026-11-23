package tools

import "fmt"

// Permission modes (players.PermissionModes). Duplicated here so tools does not
// import players.
var permissionModes = map[string]bool{"read-only": true, "edit": true, "full": true}

var (
	readTools  = map[string]bool{"read_file": true, "list_dir": true, "glob": true, "grep": true}
	writeTools = map[string]bool{"edit_file": true, "write_file": true}
)

// NeedsPermission is the API-tool approval policy (orchestra/tools/policy.py):
// read-only asks before writes and commands, edit asks before commands, full
// asks for nothing. Commands are not an OS sandbox.
func NeedsPermission(mode, tool string) (bool, error) {
	if !permissionModes[mode] {
		return false, &ToolError{Message: fmt.Sprintf("Unknown permission mode: %s", mode)}
	}
	if !readTools[tool] && !writeTools[tool] && tool != "run_command" {
		return false, &ToolError{Message: fmt.Sprintf("Unknown tool: %s", tool)}
	}
	return mode != "full" && (tool == "run_command" || (mode == "read-only" && writeTools[tool])), nil
}
