import json
import tempfile
import unittest
from pathlib import Path


class StoreTests(unittest.TestCase):
    def setUp(self):
        from orchestra.app.store import Store
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.project = self.root / 'project'
        self.project.mkdir()
        self.store = Store(self.root / 'data')
        self.addCleanup(lambda: self.store.close())

    def session(self):
        project = self.store.add_project(str(self.project))
        return self.store.create_session(project['id'], 'codex', None, 'edit')

    def test_project_dedup_and_session_survive_reopen(self):
        from orchestra.app.store import Store
        session = self.session()
        self.assertEqual(len(self.store.projects()), 1)
        self.store.add_project(str(self.project / '.'))
        self.assertEqual(len(self.store.projects()), 1)
        self.store.close()
        self.store = Store(self.root / 'data')
        self.assertEqual(self.store.session(session['id'])['project_id'], session['project_id'])

    def test_events_state_and_replay_offset_are_durable(self):
        session = self.session()
        sid = session['id']
        first = self.store.append(sid, {'type': 'text.delta', 'text': 'a'}, {'history': ['one']})
        second = self.store.append(sid, {'type': 'text.delta', 'text': 'b'}, {'history': ['two']})
        self.assertEqual([first['seq'], second['seq']], [1, 2])
        self.assertEqual(self.store.events(sid, after=1), [second])
        self.assertEqual(self.store.player_state(sid), {'history': ['two']})
        lines = (self.root / 'data' / 'sessions' / f'{sid}.jsonl').read_text().splitlines()
        self.assertEqual([json.loads(line)['text'] for line in lines], ['a', 'b'])

    def test_partial_transcript_mirror_is_repaired_from_journal(self):
        from orchestra.app.store import Store
        sid = self.session()['id']
        self.store.append(sid, {'type': 'text.delta', 'text': 'durable'})
        self.store.close()
        path = self.root / 'data' / 'sessions' / f'{sid}.jsonl'
        path.write_bytes(b'{partial')
        self.store = Store(self.root / 'data')
        self.assertEqual(json.loads(path.read_text())['text'], 'durable')

    def test_startup_keeps_complete_transcript_file_untouched(self):
        from orchestra.app.store import Store
        sid = self.session()['id']
        self.store.append(sid, {'type': 'text.delta', 'text': 'keep'})
        path = self.root / 'data' / 'sessions' / f'{sid}.jsonl'
        stamp = path.stat().st_mtime_ns
        self.store.close()
        self.store = Store(self.root / 'data')
        self.assertEqual(path.stat().st_mtime_ns, stamp)

    def test_restart_marks_running_session_interrupted_once(self):
        from orchestra.app.store import Store
        sid = self.session()['id']
        self.store.update_session(sid, status='running')
        self.store.close()
        self.store = Store(self.root / 'data')
        self.assertEqual(self.store.session(sid)['status'], 'interrupted')
        self.assertEqual(self.store.events(sid)[-1]['stop_reason'], 'interrupted')
        self.store.close()
        self.store = Store(self.root / 'data')
        self.assertEqual(len(self.store.events(sid)), 1)

    def test_terminal_event_and_status_are_atomic_across_restart(self):
        from orchestra.app.store import Store
        sid = self.session()['id']
        self.store.update_session(sid, status='running')
        self.store.append(sid, {'type': 'turn.completed', 'stop_reason': 'end_turn'})
        self.store.close()
        self.store = Store(self.root / 'data')
        self.assertEqual(self.store.session(sid)['status'], 'idle')
        self.assertEqual([e['stop_reason'] for e in self.store.events(sid)], ['end_turn'])

    def test_settings_reject_unknown_keys_and_never_accept_secrets(self):
        self.store.set_settings({'default_player': 'claude', 'permission': 'read-only'})
        self.assertEqual(self.store.settings()['default_player'], 'claude')
        for data in ({'api_key': 'never-store'}, {'permission': 'bad'}, {'default_player': 'fake'}):
            with self.subTest(data=data), self.assertRaises(ValueError):
                self.store.set_settings(data)

    def test_invalid_paths_ids_and_agents_are_rejected(self):
        for path in (self.root / 'missing', self.root / 'not-a-directory'):
            with self.assertRaises(ValueError):
                self.store.add_project(str(path))
        with self.assertRaises(KeyError):
            self.store.session('../escape')
        project = self.store.add_project(str(self.project))
        with self.assertRaises(ValueError):
            self.store.create_session(project['id'], 'unknown', None, 'edit')


if __name__ == '__main__':
    unittest.main()
