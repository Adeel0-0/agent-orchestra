"""Public JSON schemas supplied to tool-capable models."""


def _tool(name, description, properties, required=()):
    return {'type': 'function', 'function': {'name': name, 'description': description,
            'parameters': {'type': 'object', 'properties': {key: {'type': 'string', 'description': value}
                for key, value in properties.items()}, 'required': list(required), 'additionalProperties': False}}}


TOOLS = [
    _tool('read_file', 'Read a UTF-8 file (up to 1 MiB; output capped).', {'path': 'Project-relative path'}, ['path']),
    _tool('list_dir', 'List one directory, at most 1000 entries.', {'path': 'Directory, default project root'}),
    _tool('glob', 'Find files using a relative glob; ignores common generated directories.', {'pattern': 'Glob, e.g. **/*.py', 'path': 'Starting directory, default project root'}, ['pattern']),
    _tool('grep', 'Search UTF-8 files for literal text (not regex), output path:line:text.', {'pattern': 'Literal text', 'path': 'File or directory, default project root'}, ['pattern']),
    _tool('write_file', 'Create or overwrite a UTF-8 file; creates parent directories.', {'path': 'Project-relative path', 'content': 'Full file content'}, ['path', 'content']),
    _tool('edit_file', 'Replace exactly one non-empty matching string; fails without changing ambiguous matches.', {'path': 'Project-relative path', 'old': 'Exact unique text', 'new': 'Replacement text'}, ['path', 'old', 'new']),
    _tool('run_command', 'Run a shell command in the project with a 30-second timeout and output cap. Normal OS access; no sandbox.', {'command': 'PowerShell on Windows, /bin/sh elsewhere'}, ['command']),
]
