"""Exact loopback authority and launch-token checks for HTTP and WebSocket."""
import secrets


class LocalSecurity:
    def __init__(self, app, port, token, public=None):
        self.app, self.token = app, token
        self.host = f'127.0.0.1:{port}'.encode('ascii')
        self.origin = b'http://' + self.host
        self.public = public

    async def __call__(self, scope, receive, send):
        if scope['type'] not in {'http', 'websocket'}:
            return await self.app(scope, receive, send)
        headers = {}
        for key, value in scope['headers']:
            headers.setdefault(key.lower(), []).append(value)
        authority_ok = headers.get(b'host') == [self.host] and (
            b'origin' not in headers or headers[b'origin'] == [self.origin])
        authority_ok &= not any(k == b'forwarded' or k.startswith(b'x-forwarded-') for k in headers)
        supplied = ''
        auth = headers.get(b'authorization', [])
        if len(auth) == 1 and auth[0].startswith(b'Bearer '):
            supplied = auth[0][7:].decode('ascii', errors='replace')
        if scope['type'] == 'websocket' and not supplied:
            protocols = headers.get(b'sec-websocket-protocol', [])
            if len(protocols) == 1:
                candidates = [p.strip() for p in protocols[0].decode('ascii', errors='replace').split(',')]
                tokens = [p[len('orchestra.token.'):] for p in candidates if p.startswith('orchestra.token.')]
                if 'orchestra' in candidates and len(tokens) == 1:
                    supplied = tokens[0]
        authenticated = supplied.isascii() and secrets.compare_digest(supplied, self.token)
        public = self.public(scope) if self.public is not None else False
        if not authority_ok or not (authenticated or public):
            if scope['type'] == 'websocket':
                await send({'type': 'websocket.close', 'code': 1008})
            else:
                status = 403 if not authority_ok else 401
                await send({'type': 'http.response.start', 'status': status,
                            'headers': [(b'content-type', b'application/json'), (b'cache-control', b'no-store')]})
                await send({'type': 'http.response.body', 'body': b'{"error":"Local authorization required"}'})
            return

        async def secure_send(message):
            if message['type'] == 'http.response.start':
                message['headers'] = [*message.get('headers', []), (b'cache-control', b'no-store'),
                    (b'x-content-type-options', b'nosniff'), (b'referrer-policy', b'no-referrer'),
                    (b'content-security-policy', b"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws://" + self.host + b"; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")]
            await send(message)
        await self.app(scope, receive, secure_send)
