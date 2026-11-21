"""Offline browser acceptance server. Never imported by the product launcher."""
import asyncio
import json
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

import uvicorn
from orchestra.app.launcher import reserve_socket
from orchestra.app.server import create_app
from orchestra.app.store import Store
from orchestra.players.base import Event
from orchestra.players.registry import PlayerHealth
from orchestra.players.openrouter_client import OpenRouterClient


async def main():
    with tempfile.TemporaryDirectory(prefix='orchestra-ui-test-') as temporary:
        root = Path(temporary)
        project = root / 'Sample project'
        project.mkdir()
        (project / 'greeting.py').write_text('print("hello")\n')
        for args in (['init'], ['add', 'greeting.py'], ['-c','user.name=Test','-c','user.email=test@example.invalid','commit','-m','fixture']):
            subprocess.run(['git','-C',str(project),*args],check=True,capture_output=True)
        class Player:
            def __init__(self, session, cwd):
                self.session_id=session['backend_session_id'] or 'offline-test'
                self.cwd=cwd
                self.pending=None
            async def send(self, prompt):
                yield Event('session.started', {'backend_session_id':self.session_id,'model':'fixture-small'})
                yield Event('text.delta', {'text':'This is an **offline acceptance test**. I will update `greeting.py` after your approval.'})
                yield Event('tool.started', {'id':'edit-1','name':'edit_file','input':{'path':'greeting.py','old':'hello','new':'goodbye'}})
                self.pending=asyncio.get_running_loop().create_future()
                yield Event('permission.requested', {'request_id':'edit-request','tool':'edit_file','input':{'path':'greeting.py','old':'hello','new':'goodbye'},'reason':'Approve this test edit.'})
                allow=await self.pending
                if allow:
                    (self.cwd/'greeting.py').write_text('print("goodbye")\n')
                    yield Event('file.changed', {'path':'greeting.py','kind':'update'})
                yield Event('tool.output', {'id':'edit-1','chunk':'Updated greeting.py' if allow else 'Edit denied'})
                yield Event('tool.finished', {'id':'edit-1','ok':allow})
                yield Event('text.delta', {'text':'The file now prints `goodbye`.' if allow else 'No files changed.'})
                yield Event('usage', {'input_tokens':120,'output_tokens':32,'cost_usd':0})
                yield Event('turn.completed', {'stop_reason':'end_turn'})
            async def answer_permission(self, rid, allow):
                self.pending.set_result(allow)
            async def cancel(self):
                if self.pending and not self.pending.done(): self.pending.cancel()
        async def models(_):
            return [{'id':'fixture-small','name':'Small offline model'},{'id':'fixture-large','name':'Large offline model'}]
        health=lambda:[PlayerHealth('codex','Codex (offline test)',sys.executable,'fixture',True,None),
                       PlayerHealth('openrouter','OpenRouter',None,None,False,'Configure a key privately in a terminal')]
        with patch('orchestra.players.registry.check_all',health),patch.object(OpenRouterClient,'models',models):
            with reserve_socket() as sock:
                port=sock.getsockname()[1]
                app=create_app(store=Store(root/'data'),port=port,web_dist=Path(__file__).resolve().parents[1]/'orchestra/app/web/dist',
                               factory=lambda session,cwd:Player(session,cwd),folder_picker=lambda:str(project))
                server=uvicorn.Server(uvicorn.Config(app,log_level='critical',access_log=False,timeout_graceful_shutdown=2))
                task=asyncio.create_task(server.serve(sockets=[sock]))
                while not server.started:
                    if task.done(): await task;return
                    await asyncio.sleep(.02)
                print('ORCHESTRA_E2E_READY '+json.dumps({'url':f'http://127.0.0.1:{port}/#bootstrap='+app.state.bootstrap.issue()}),flush=True)
                try:
                    await asyncio.to_thread(sys.stdin.readline)
                    server.should_exit=True
                    await asyncio.wait_for(task,10)
                finally:
                    app.state.store.close()


if __name__=='__main__':
    asyncio.run(main())
