"""Own agent turns independently of HTTP requests and WebSocket subscribers."""
import asyncio
import os
import uuid
from pathlib import Path

from ..bus import LockManager
from ..players import base
from ..players.registry import get_player


class Conflict(RuntimeError):
    pass


class SlowSubscriber(RuntimeError):
    pass


def make_player(session, cwd):
    cls = get_player(session['player'])
    return cls(cwd=cwd, model=session['model'], permission=session['permission'],
               session_id=session['backend_session_id'] if session['player'] != 'openrouter' else None)


class SessionManager:
    def __init__(self, store, factory=make_player):
        self.store, self.factory = store, factory
        self._players, self._tasks, self._subscribers, self._paths, self._permissions = {}, {}, {}, {}, {}
        self._leases = {}
        self._launch = uuid.uuid4().hex
        self._closed = False

    def _snapshot(self, sid):
        player = self._players.get(sid)
        return player.snapshot_state() if player and hasattr(player, 'snapshot_state') else None

    def publish(self, sid, event):
        record = self.store.append(sid, event, self._snapshot(sid))
        for queue in tuple(self._subscribers.get(sid, ())):
            if queue.full():
                while not queue.empty():
                    queue.get_nowait()
                queue.put_nowait(None)
                self._subscribers[sid].discard(queue)
            else:
                queue.put_nowait(record)
        return record

    def start(self, sid, prompt):
        if self._closed:
            raise Conflict('Session manager is shutting down')
        if not isinstance(prompt, str) or not prompt.strip() or len(prompt.encode('utf-8')) > 256 * 1024:
            raise ValueError('prompt must contain 1 to 262144 UTF-8 bytes')
        session = self.store.session(sid)
        if sid in self._tasks and not self._tasks[sid].done():
            raise Conflict('A turn is already running in this session')
        try:
            cwd = Path(session['worktree'] or self.store.project(session['project_id'])['path']).resolve(strict=True)
        except (OSError, ValueError):
            raise ValueError('Project folder is missing or inaccessible') from None
        if not cwd.is_dir():
            raise ValueError('Project path is no longer a folder')
        for blocked in self.store.blocked_sessions():
            path = Path(blocked['worktree'] or blocked['path']).resolve()
            if cwd.is_relative_to(path) or path.is_relative_to(cwd):
                raise Conflict('Project has an interrupted turn or failed cleanup. Stop leftover processes, then confirm_stopped through the cancel endpoint.')
        for path in self._paths.values():
            if cwd.is_relative_to(path) or path.is_relative_to(cwd):
                raise Conflict('Another session is running in this project or an overlapping folder')
        lock = LockManager(cwd / '.orchestra')
        owner = 'session-' + sid + '-' + self._launch
        if not lock.acquire(sid, owner, scope='local app turn', lease_seconds=120):
            raise Conflict('The project writer lock is held by another agent')
        try:
            if sid not in self._players:
                player = self.factory(session, cwd)
                state = self.store.player_state(sid)
                if state and hasattr(player, 'restore_state'):
                    player.restore_state(state)
                self._players[sid] = player
            self.publish(sid, {'type': 'message.user', 'text': prompt})
            self._paths[sid] = cwd
            self._leases[sid] = (lock, owner)
            self._tasks[sid] = asyncio.create_task(self._run(sid, prompt, lock, owner), name='orchestra-turn-' + sid)
        except BaseException:
            lock.release(owner)
            raise

    async def _heartbeat(self, sid, lock, owner):
        while True:
            await asyncio.sleep(30)
            try:
                renewed = lock.heartbeat(owner, extend_seconds=120)
            except Exception:
                renewed = False
            if not renewed:
                try:
                    self.publish(sid, base.error_event('Project writer lease was lost; turn stopped.').to_dict())
                except Exception:
                    pass
                finally:
                    task = self._tasks.get(sid)
                    if task:
                        task.cancel()
                return

    async def _run(self, sid, prompt, lock, owner):
        player = self._players[sid]
        heartbeat = asyncio.create_task(self._heartbeat(sid, lock, owner))
        completed, errored, status = False, False, 'idle'
        stream = player.send(prompt)
        try:
            async for event in stream:
                if event.type == base.PERMISSION_REQUESTED:
                    self._permissions.setdefault(sid, set()).add(event.data['request_id'])
                    self.store.update_session(sid, status='waiting_permission')
                if event.type == base.ERROR:
                    errored = True
                if event.type == base.TURN_COMPLETED:
                    completed = True
                    reason = event.data.get('stop_reason')
                    status = 'error' if reason == 'error' or errored else ('cancelled' if reason == 'cancelled' else 'idle')
                self.publish(sid, event.to_dict())
        except asyncio.CancelledError:
            status = 'cancelled'
            raise
        except Exception as exc:
            status = 'error'
            self.publish(sid, base.error_event(f'Turn failed: {type(exc).__name__}: {exc}').to_dict())
        finally:
            cleanup = await asyncio.gather(stream.aclose(), player.cancel(), return_exceptions=True)
            heartbeat.cancel()
            await asyncio.gather(heartbeat, return_exceptions=True)
            try:
                if any(isinstance(e, BaseException) for e in cleanup):
                    status = 'cleanup_failed'
                if not completed:
                    if status not in {'cancelled', 'cleanup_failed'}:
                        status = 'error'
                    self.publish(sid, {'type': base.TURN_COMPLETED, 'stop_reason': status if status == 'cancelled' else 'error'})
                self.store.update_session(sid, status=status)
                if status == 'cleanup_failed':
                    self.publish(sid, base.error_event('Player cleanup failed; project is blocked until leftover processes are stopped and confirmed.').to_dict())
            finally:
                if status == 'cleanup_failed':
                    self._permissions.pop(sid, None)
                    try:
                        lock.heartbeat(owner, extend_seconds=86400)
                    except Exception:
                        pass
                else:
                    self._release(sid)

    def _release(self, sid):
        self._permissions.pop(sid, None)
        self._paths.pop(sid, None)
        lease = self._leases.pop(sid, None)
        if lease:
            lease[0].release(lease[1])  # No handoff event or automatic shipping.

    async def wait(self, sid):
        task = self._tasks.get(sid)
        if task:
            await asyncio.gather(task, return_exceptions=True)

    async def cancel(self, sid, confirm_stopped=False):
        session = self.store.session(sid)
        if not isinstance(confirm_stopped, bool):
            raise ValueError('confirm_stopped must be boolean')
        task = self._tasks.get(sid)
        if (task is None or task.done()) and session['status'] in {'cleanup_failed', 'interrupted'}:
            if not confirm_stopped:
                raise Conflict('Stop leftover processes, then repeat cancel with confirm_stopped=true')
            cwd = session['worktree'] or self.store.project(session['project_id'])['path']
            lock = LockManager(Path(cwd) / '.orchestra')
            current = lock.get_current_lock()
            if current and current.get('task_id') == sid and str(current.get('agent_id', '')).startswith('session-'):
                lock.release(current['agent_id'])
            self._release(sid)
            self.store.update_session(sid, status='cancelled')
            self.publish(sid, {'type': base.NOTICE, 'message': 'User confirmed leftover processes are stopped.'})
            return
        if task and not task.done():
            self.store.update_session(sid, status='cancelling')
            # Task cancellation closes the player's generator and its process/tool tree.
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            # A task cancelled before its first instruction never enters its finally block.
            if sid in self._leases:
                try:
                    await self._players[sid].cancel()
                    self.publish(sid, {'type': base.TURN_COMPLETED, 'stop_reason': 'cancelled'})
                    self.store.update_session(sid, status='cancelled')
                except Exception:
                    self.publish(sid, {'type': base.TURN_COMPLETED, 'stop_reason': 'error'})
                    self.store.update_session(sid, status='cleanup_failed')
                    self.publish(sid, base.error_event('Player cleanup failed; project is blocked.').to_dict())
                    lease = self._leases[sid]
                    try:
                        lease[0].heartbeat(lease[1], extend_seconds=86400)
                    except Exception:
                        pass
                else:
                    self._release(sid)

    async def answer(self, sid, rid, allow):
        self.store.session(sid)
        if not isinstance(allow, bool):
            raise ValueError('allow must be boolean')
        if rid not in self._permissions.get(sid, set()):
            raise Conflict('Permission request is not pending in this session')
        await self._players[sid].answer_permission(rid, allow)
        self._permissions[sid].discard(rid)
        self.store.update_session(sid, status='running')
        self.publish(sid, {'type': 'permission.answered', 'request_id': rid, 'allow': allow})

    async def events(self, sid, after=0):
        latest = self.store.last_seq(sid)
        if not isinstance(after, int) or not 0 <= after <= latest:
            raise ValueError('after must be a sequence between zero and the last stored event')
        queue = asyncio.Queue(maxsize=128)
        subscribers = self._subscribers.setdefault(sid, set())
        subscribers.add(queue)
        try:
            while True:
                page = self.store.events(sid, after=after)
                if not page:
                    break
                for record in page:
                    after = record['seq']
                    yield record
            while True:
                record = await queue.get()
                if record is None:
                    raise SlowSubscriber('Subscriber fell behind; reconnect with last received seq')
                if record['seq'] > after:
                    after = record['seq']
                    yield record
        finally:
            subscribers.discard(queue)

    async def close(self):
        self._closed = True
        for sid in list(self._tasks):
            if not self._tasks[sid].done():
                await self.cancel(sid)
        for queues in self._subscribers.values():
            for queue in queues:
                while not queue.empty():
                    queue.get_nowait()
                queue.put_nowait(None)
