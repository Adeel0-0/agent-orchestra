"""API-tool approval policy. Commands are not an OS sandbox."""

READ_TOOLS = {'read_file', 'list_dir', 'glob', 'grep'}
WRITE_TOOLS = {'edit_file', 'write_file'}


def needs_permission(mode: str, tool: str) -> bool:
    from ..players.base import PERMISSION_MODES
    if mode not in PERMISSION_MODES:
        raise ValueError(f'Unknown permission mode: {mode}')
    if tool not in READ_TOOLS | WRITE_TOOLS | {'run_command'}:
        raise ValueError(f'Unknown tool: {tool}')
    return mode != 'full' and (tool == 'run_command' or (mode == 'read-only' and tool in WRITE_TOOLS))
