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

# Numeric tool inputs must stay numeric through the API schema and approval card.
TOOLS[0]['function']['parameters']['properties'].update({
    'offset': {'type': 'integer', 'minimum': 0, 'description': 'Zero-based starting line index (default 0)'},
    'limit': {'type': 'integer', 'minimum': 1, 'maximum': 2000, 'description': 'Maximum lines to read (default 200); output reports next offset and remaining lines'},
})
TOOLS[-1]['function']['parameters']['properties']['timeout'] = {
    'type': 'number', 'exclusiveMinimum': 0, 'maximum': 600, 'description': 'Command deadline in seconds, default 30, maximum 600'}
TOOLS[-1]['function']['description'] = 'Run an approved shell command in the project; timeout up to 600 seconds, capped output. Secret-suffix environment variables are removed. Normal OS access; no sandbox.'
