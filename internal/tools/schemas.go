package tools

// Schemas returns the public JSON schemas supplied to tool-capable models
// (orchestra/tools/schemas.py). Each call returns a fresh copy.
func Schemas() []any {
	readFile := tool("read_file", "Read a UTF-8 file (up to 1 MiB; output capped).",
		[][2]string{{"path", "Project-relative path"}}, "path")
	props := readFile["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	// Numeric tool inputs must stay numeric through the API schema and approval card.
	props["offset"] = map[string]any{"type": "integer", "minimum": 0, "description": "Zero-based starting line index (default 0)"}
	props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 2000,
		"description": "Maximum lines to read (default 200); output reports next offset and remaining lines"}

	run := tool("run_command", "Run an approved shell command in the project; timeout up to 600 seconds, capped output. "+
		"Secret-suffix environment variables are removed. Normal OS access; no sandbox.",
		[][2]string{{"command", "PowerShell on Windows, /bin/sh elsewhere"}}, "command")
	runProps := run["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	runProps["timeout"] = map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 600,
		"description": "Command deadline in seconds, default 30, maximum 600"}

	return []any{
		readFile,
		tool("list_dir", "List one directory, at most 1000 entries.",
			[][2]string{{"path", "Directory, default project root"}}),
		tool("glob", "Find files using a relative glob; ignores common generated directories.",
			[][2]string{{"pattern", "Glob, e.g. **/*.py"}, {"path", "Starting directory, default project root"}}, "pattern"),
		tool("grep", "Search UTF-8 files for literal text (not regex), output path:line:text.",
			[][2]string{{"pattern", "Literal text"}, {"path", "File or directory, default project root"}}, "pattern"),
		tool("write_file", "Create or overwrite a UTF-8 file; creates parent directories.",
			[][2]string{{"path", "Project-relative path"}, {"content", "Full file content"}}, "path", "content"),
		tool("edit_file", "Replace exactly one non-empty matching string; fails without changing ambiguous matches.",
			[][2]string{{"path", "Project-relative path"}, {"old", "Exact unique text"}, {"new", "Replacement text"}},
			"path", "old", "new"),
		run,
	}
}

func tool(name, description string, properties [][2]string, required ...string) map[string]any {
	props := map[string]any{}
	for _, p := range properties {
		props[p[0]] = map[string]any{"type": "string", "description": p[1]}
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": props, "required": req,
			"additionalProperties": false},
	}}
}
