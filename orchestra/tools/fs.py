"""Bounded text tools. Resolve every accessed path, including search results."""
import os
from fnmatch import fnmatchcase
from functools import lru_cache
from pathlib import Path, PureWindowsPath

from . import ToolResult

OUTPUT_LIMIT = 16000
FILE_LIMIT = 1024 * 1024


class ScanLimitReached(Exception):
    pass


def bounded(text: str, limit: int = OUTPUT_LIMIT) -> str:
    return text if len(text) <= limit else text[:limit] + '\n[output truncated]'


def string_arg(args: dict, name: str, default=None) -> str:
    value = args.get(name, default)
    if not isinstance(value, str):
        raise ValueError(f'{name} must be a string')
    return value


def glob_matches(path: Path, pattern: str) -> bool:
    """Anchored path glob: * stays in a segment, ** spans zero or more segments."""
    parts, patterns = path.parts, Path(pattern).parts

    @lru_cache(None)
    def match(i, j):
        if j == len(patterns):
            return i == len(parts)
        if patterns[j] == '**':
            return match(i, j + 1) or (i < len(parts) and match(i + 1, j))
        return i < len(parts) and fnmatchcase(parts[i], patterns[j]) and match(i + 1, j + 1)

    return match(0, 0)


class FileTools:
    def __init__(self, root: Path, scan_limit=10000):
        self.root = Path(root).resolve(strict=True)
        if not self.root.is_dir():
            raise ValueError('Project root must be a directory')
        self.scan_limit = scan_limit

    def resolve(self, path: str, *, write=False) -> Path:
        if not isinstance(path, str) or not path or '\x00' in path:
            raise ValueError('path must be a non-empty string')
        target = (self.root / path).resolve()
        if not target.is_relative_to(self.root):
            raise ValueError('Path leaves the project root')
        # Refuse NTFS alternate streams and device/extended paths.
        if os.name == 'nt' and (':' in str(target.relative_to(self.root)) or path.startswith(('\\\\?', '\\\\.'))):
            raise ValueError('Device paths and alternate streams are not supported')
        if os.name == 'nt' and any(PureWindowsPath(part).is_reserved() for part in target.parts[1:]):
            raise ValueError('Reserved Windows path')
        if write:
            # Check both lexical and resolved paths, including junction/symlink aliases.
            for candidate in (self.root / path, target):
                if any(part.rstrip(' .').casefold() in {'.git', '.orchestra'} for part in candidate.parts):
                    raise ValueError('Writes to .git and .orchestra metadata are forbidden')
            if target.is_file() and target.stat().st_nlink > 1:
                raise ValueError('Writes to hardlinked files are forbidden; the target may alias protected metadata')
        return target

    def read(self, path: Path) -> str:
        # Open once; a bounded read avoids loading huge files into memory.
        with path.open('rb') as f:
            raw = f.read(FILE_LIMIT + 1)
        if len(raw) > FILE_LIMIT:
            raise ValueError('File exceeds 1 MiB text-tool limit')
        if b'\x00' in raw:
            raise ValueError('Binary files are not supported')
        return raw.decode('utf-8')

    def files(self, start: Path):
        scanned = 0
        for directory, dirs, files in os.walk(start, followlinks=False):
            dirs[:] = sorted(d for d in dirs if d not in {'.git', '.orchestra', 'node_modules', '.venv', '__pycache__'}
                             and not (Path(directory) / d).is_symlink()
                             and not (hasattr(os.path, 'isjunction') and os.path.isjunction(Path(directory) / d))
                             and (Path(directory) / d).resolve().is_relative_to(self.root))
            for name in sorted(files):
                scanned += 1
                if scanned > self.scan_limit:
                    raise ScanLimitReached
                p = Path(directory) / name
                if not p.is_symlink() and p.resolve().is_relative_to(self.root):
                    yield p

    def execute(self, name: str, args: dict) -> ToolResult:
        if name not in {'read_file', 'list_dir', 'glob', 'grep', 'write_file', 'edit_file'}:
            raise ValueError(f'Unknown tool: {name}')
        path = self.resolve(string_arg(args, 'path', '.'), write=name in {'write_file', 'edit_file'})
        if name == 'read_file':
            text = self.read(path)
            offset, limit = args.get('offset', 0), args.get('limit', 200)
            if type(offset) is not int or offset < 0 or type(limit) is not int or not 1 <= limit <= 2000:
                raise ValueError('offset must be a non-negative line index; limit must be 1 to 2000 lines')
            lines = text.splitlines(keepends=True)
            selected, size, end = [], 0, min(offset, len(lines))
            for line in lines[offset:offset + limit]:
                if size + len(line) > OUTPUT_LIMIT - 200:
                    if not selected:
                        return ToolResult(bounded(line, OUTPUT_LIMIT - 200) + f'\n[line {offset + 1} exceeds output cap; use an approved command to inspect the long line]')
                    break
                selected.append(line)
                size += len(line)
                end += 1
            if 'offset' not in args and 'limit' not in args and end == len(lines):
                return ToolResult(text)
            footer = f'\n[lines {offset + 1 if selected else end}-{end} of {len(lines)}; {len(lines) - end} lines remain; next offset: {end}]'
            return ToolResult(''.join(selected) + footer)
        if name == 'list_dir':
            entries = []
            with os.scandir(path) as scan:
                for entry in scan:
                    if len(entries) >= 1000:
                        entries.append('[listing truncated]')
                        break
                    entries.append(entry.name + ('/' if entry.is_dir(follow_symlinks=False) else ''))
            return ToolResult(bounded('\n'.join(sorted(entries))))
        if name in {'glob', 'grep'}:
            pattern = string_arg(args, 'pattern')
            if name == 'glob' and (Path(pattern).is_absolute() or '..' in Path(pattern).parts):
                raise ValueError('glob pattern must stay inside the project')
            lines, size = [], 0
            def safe_files():
                try:
                    yield from ([path] if path.is_file() else self.files(path))
                except ScanLimitReached:
                    yield None
            for p in safe_files():
                if p is None:
                    lines.append(f'[scan truncated after {self.scan_limit} files; narrow the search path]')
                    break
                rel = p.relative_to(self.root).as_posix()
                if name == 'glob':
                    matches = [rel] if glob_matches(p.relative_to(path), pattern) else []
                else:
                    try:
                        matches = [f'{rel}:{i}:{line}' for i, line in enumerate(self.read(p).splitlines(), 1) if pattern in line]
                    except (UnicodeError, OSError, ValueError):
                        continue
                for line in matches:
                    lines.append(line)
                    size += len(line) + 1
                    if size > OUTPUT_LIMIT:
                        return ToolResult(bounded('\n'.join(lines)))
            return ToolResult(bounded('\n'.join(lines)))
        existed = path.exists()
        if name == 'write_file':
            text = string_arg(args, 'content')
        else:
            old, new = string_arg(args, 'old'), string_arg(args, 'new')
            text = self.read(path)
            if not old or text.count(old) != 1:
                raise ValueError('edit_file requires exactly one non-empty match; file unchanged')
            text = text.replace(old, new, 1)
        if len(text.encode('utf-8')) > FILE_LIMIT:
            raise ValueError('Write exceeds 1 MiB text-tool limit')
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding='utf-8', newline='')
        return ToolResult('File written.', path=path.relative_to(self.root).as_posix(), kind='update' if existed else 'add')
