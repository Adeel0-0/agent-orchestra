"""SQLite journal with atomic player snapshots and repairable JSONL transcripts."""
import json
import logging
import os
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from .errors import RequestError, NotFound

from ..players.base import PERMISSION_MODES
from ..players.registry import PLAYERS


class Store:
    def __init__(self, root=None):
        self.root = Path(root) if root is not None else Path.home() / '.orchestra'
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.transcripts = self.root / 'sessions'
        self.transcripts.mkdir(exist_ok=True, mode=0o700)
        self._guard = threading.RLock()
        self._lock = (self.root / 'server.lock').open('a+b')
        self.db = None
        try:
            self._lock.seek(0)
            if not self._lock.read(1):
                self._lock.write(b'0')
                self._lock.flush()
            self._lock.seek(0)
            if os.name == 'nt':
                import msvcrt
                msvcrt.locking(self._lock.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(self._lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self._lock.close()
            raise RuntimeError('Another Orchestra server owns this data directory.') from None
        try:
            self.db = sqlite3.connect(self.root / 'orchestra.db', check_same_thread=False)
            self.db.row_factory = sqlite3.Row
            self.db.execute('PRAGMA foreign_keys=ON')
            self.db.execute('PRAGMA journal_mode=WAL')
            self.db.executescript('''
                CREATE TABLE IF NOT EXISTS projects (
                    id TEXT PRIMARY KEY, path TEXT UNIQUE NOT NULL, name TEXT NOT NULL, last_opened REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS sessions (
                    id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id),
                    player TEXT NOT NULL, model TEXT, title TEXT NOT NULL, worktree TEXT,
                    backend_session_id TEXT, status TEXT NOT NULL, created REAL NOT NULL,
                    updated REAL NOT NULL, permission TEXT NOT NULL, base_commit TEXT);
                CREATE TABLE IF NOT EXISTS events (
                    session_id TEXT NOT NULL REFERENCES sessions(id), seq INTEGER NOT NULL,
                    payload TEXT NOT NULL, PRIMARY KEY(session_id,seq));
                CREATE TABLE IF NOT EXISTS player_states (
                    session_id TEXT PRIMARY KEY REFERENCES sessions(id), payload TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
            ''')
            for session in self.db.execute('SELECT id,status FROM sessions').fetchall():
                if session['status'] in {'running', 'waiting_permission', 'cancelling'}:
                    self.append(session['id'], {'type': 'turn.completed', 'stop_reason': 'interrupted'})
                self._repair(session['id'])
        except BaseException:
            self.close()
            raise

    def close(self):
        if self.db is not None:
            self.db.close()
            self.db = None
        if not self._lock.closed:
            # Closing the descriptor releases the OS lock; never remove a live lock file.
            self._lock.close()

    def projects(self):
        with self._guard:
            return [dict(r) for r in self.db.execute('SELECT * FROM projects ORDER BY last_opened DESC')]

    def project(self, pid):
        with self._guard:
            row = self.db.execute('SELECT * FROM projects WHERE id=?', (pid,)).fetchone()
        if row is None:
            raise NotFound('Unknown project')
        return dict(row)

    def add_project(self, path):
        if not isinstance(path, str) or not path.strip():
            raise RequestError('path must be a non-empty folder path')
        try:
            folder = Path(path).resolve(strict=True)
        except (OSError, ValueError):
            raise RequestError('Project folder does not exist') from None
        if not folder.is_dir():
            raise RequestError('Project path must be a folder')
        normalized = os.path.normcase(str(folder))
        with self._guard, self.db:
            self.db.execute('''INSERT INTO projects VALUES (?,?,?,?)
                ON CONFLICT(path) DO UPDATE SET last_opened=excluded.last_opened''',
                (uuid.uuid4().hex, normalized, folder.name or str(folder), time.time()))
            return dict(self.db.execute('SELECT * FROM projects WHERE path=?', (normalized,)).fetchone())

    def create_session(self, pid, player, model, permission, title='New session', base_commit=None):
        self.project(pid)
        if not isinstance(player, str) or player not in PLAYERS:
            raise RequestError('Unknown player')
        if not isinstance(permission, str) or permission not in PERMISSION_MODES:
            raise RequestError('Unknown permission mode')
        if model is not None and (not isinstance(model, str) or not model.strip() or len(model) > 256):
            raise RequestError('model must be a non-empty string or null')
        if player == 'openrouter' and not model:
            raise RequestError('OpenRouter requires a model')
        if not isinstance(title, str) or not title.strip() or len(title) > 200:
            raise RequestError('title must contain 1 to 200 characters')
        sid, now = uuid.uuid4().hex, time.time()
        with self._guard, self.db:
            self.db.execute('INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?,?,?)',
                (sid, pid, player, model, title, None, None, 'idle', now, now, permission, base_commit))
        return self.session(sid)

    def session(self, sid):
        with self._guard:
            row = self.db.execute('SELECT * FROM sessions WHERE id=?', (sid,)).fetchone()
        if row is None:
            raise NotFound('Unknown session')
        return dict(row)

    def sessions(self, pid):
        self.project(pid)
        with self._guard:
            return [dict(r) for r in self.db.execute('SELECT * FROM sessions WHERE project_id=? ORDER BY updated DESC', (pid,))]

    def update_session(self, sid, **values):
        self.session(sid)
        if set(values) - {'status', 'title', 'backend_session_id', 'base_commit'}:
            raise RequestError('Unsupported session update')
        values['updated'] = time.time()
        with self._guard, self.db:
            self.db.execute('UPDATE sessions SET ' + ','.join(f'{k}=?' for k in values) + ' WHERE id=?', [*values.values(), sid])

    def append(self, sid, event, state=None):
        self.session(sid)
        with self._guard, self.db:
            seq = self.db.execute('SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE session_id=?', (sid,)).fetchone()[0]
            record = {**event, 'seq': seq, 'timestamp': time.time()}
            payload = json.dumps(record, ensure_ascii=False, allow_nan=False)
            self.db.execute('INSERT INTO events VALUES (?,?,?)', (sid, seq, payload))
            if state is not None:
                self.db.execute('INSERT INTO player_states VALUES (?,?) ON CONFLICT(session_id) DO UPDATE SET payload=excluded.payload',
                                (sid, json.dumps(state, ensure_ascii=False, allow_nan=False)))
            if event.get('type') == 'session.started' and event.get('backend_session_id'):
                self.db.execute('UPDATE sessions SET backend_session_id=? WHERE id=?', (event['backend_session_id'], sid))
            if event.get('type') in {'turn.completed', 'message.user'}:
                status = 'running' if event['type'] == 'message.user' else {
                    'end_turn': 'idle', 'cancelled': 'cancelled', 'interrupted': 'interrupted'}.get(event.get('stop_reason'), 'error')
                self.db.execute('UPDATE sessions SET status=?,updated=? WHERE id=?', (status, time.time(), sid))
        try:
            with (self.transcripts / f'{sid}.jsonl').open('a', encoding='utf-8', newline='\n') as out:
                out.write(payload + '\n')
        except OSError:
            logging.getLogger(__name__).warning('Transcript mirror write failed; durable journal retained for session %s', sid)
        return record

    def events(self, sid, after=0, limit=1000):
        self.session(sid)
        if not isinstance(after, int) or after < 0 or not 1 <= limit <= 1000:
            raise RequestError('Invalid event offset or page size')
        with self._guard:
            return [json.loads(r[0]) for r in self.db.execute(
                'SELECT payload FROM events WHERE session_id=? AND seq>? ORDER BY seq LIMIT ?', (sid, after, limit))]

    def last_seq(self, sid):
        self.session(sid)
        with self._guard:
            return self.db.execute('SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id=?', (sid,)).fetchone()[0]

    def blocked_sessions(self):
        with self._guard:
            return [dict(r) for r in self.db.execute('''SELECT sessions.*, projects.path FROM sessions
                JOIN projects ON projects.id=sessions.project_id WHERE status IN ('cleanup_failed','interrupted')''')]

    def player_state(self, sid):
        self.session(sid)
        with self._guard:
            row = self.db.execute('SELECT payload FROM player_states WHERE session_id=?', (sid,)).fetchone()
        return json.loads(row[0]) if row else None

    def _repair(self, sid):
        expected = self.db.execute('SELECT COALESCE(SUM(length(CAST(payload AS BLOB))+1),0) FROM events WHERE session_id=?', (sid,)).fetchone()[0]
        target = self.transcripts / f'{sid}.jsonl'
        if target.is_file() and target.stat().st_size == expected:
            return
        temporary = self.transcripts / f'{sid}.tmp'
        with temporary.open('w', encoding='utf-8', newline='\n') as out:
            for row in self.db.execute('SELECT payload FROM events WHERE session_id=? ORDER BY seq', (sid,)):
                out.write(row[0] + '\n')
        temporary.replace(target)

    def settings(self):
        result = {'default_player': 'codex', 'permission': 'edit'}
        with self._guard:
            result.update({r[0]: json.loads(r[1]) for r in self.db.execute('SELECT key,value FROM settings')})
        return result

    def set_settings(self, values):
        if not isinstance(values, dict) or set(values) - {'default_player', 'permission'}:
            raise RequestError('Settings accept only default_player and permission; credentials use the OS keyring')
        player, permission = values.get('default_player', 'codex'), values.get('permission', 'edit')
        if not isinstance(player, str) or player not in PLAYERS or not isinstance(permission, str) or permission not in PERMISSION_MODES:
            raise RequestError('Unknown player or permission mode')
        with self._guard, self.db:
            for key, value in values.items():
                self.db.execute('INSERT INTO settings VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value', (key, json.dumps(value)))
        return self.settings()
