#!/usr/bin/env python3
"""Exercise shared-port HTTP/HTTPS, cookies and certificate renewal with real Nginx.

Run: python3 docker-compose.dokploy-https.test.py
Uses disposable Docker containers with fake application backends and a private
test CA. No production credentials, published host ports or Docker socket mounts.
"""

import base64
import hashlib
import http.client
import http.server
from http.cookies import SimpleCookie
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time


DOMAIN = 'multica.savetest.top'


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def serve():
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == '/ws':
                assert self.headers.get('Upgrade', '').lower() == 'websocket'
                assert self.headers.get('Connection', '').lower() == 'upgrade'
                accept = base64.b64encode(hashlib.sha1(
                    (self.headers['Sec-WebSocket-Key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()
                ).digest()).decode()
                self.send_response(101)
                self.send_header('Upgrade', 'websocket')
                self.send_header('Connection', 'Upgrade')
                self.send_header('Sec-WebSocket-Accept', accept)
                self.end_headers()
                return
            body = json.dumps(dict(status='ok', path=self.path, port=self.server.server_port,
                                   host=self.headers.get('Host'),
                                   cookie=self.headers.get('Cookie'),
                                   forwarded_for=self.headers.get('X-Forwarded-For'),
                                   scheme=self.headers.get('X-Forwarded-Proto'))).encode()
            self.send_response(200)
            for name in ['multica_auth', 'multica_csrf', 'multica_csrf_session']:
                suffix = '; HttpOnly' if name == 'multica_auth' else ''
                self.send_header('Set-Cookie', f'{name}=fixture; Path=/; SameSite=Strict{suffix}')
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass

    for port in [3000, 8080]:
        server = http.server.ThreadingHTTPServer(('0.0.0.0', port), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
    Path('/fixtures/backend-ready').touch()
    threading.Event().wait()


def check():
    ctx = ssl.create_default_context(cafile='/fixtures/ca.crt')

    def request(path, tls=True, port=3200, extra='', host=f'{DOMAIN}:3200'):
        with socket.create_connection(('gateway', port), timeout=3) as raw:
            connection = ctx.wrap_socket(raw, server_hostname=DOMAIN) if tls else raw
            with connection:
                fingerprint = hashlib.sha256(connection.getpeercert(binary_form=True)).hexdigest() if tls else None
                connection.sendall((f'GET {path} HTTP/1.1\r\nHost: {host}\r\n'
                                    f'{extra}Connection: close\r\n\r\n').encode())
                response = http.client.HTTPResponse(connection)
                response.begin()
                body = response.read() if response.status != 101 else b''
                headers = dict(response.getheaders())
                headers['cookies'] = response.headers.get_all('Set-Cookie', [])
                return response.status, headers, body, fingerprint

    def check_cookies(result, secure):
        cookies = SimpleCookie()
        for header in result[1]['cookies']:
            cookies.load(header)
        assert len(cookies) == 3, result
        for name, cookie in cookies.items():
            assert bool(cookie['secure']) == secure, (name, cookie)
            assert cookie['samesite'].lower() == 'strict', cookie
            assert bool(cookie['httponly']) == (name == 'multica_auth'), cookie

    for _ in range(20):
        try:
            first = request('/readyz')
            break
        except OSError:
            time.sleep(0.5)
    else:
        raise AssertionError('Nginx never became ready')
    assert first[0] == 200, first
    for tls, host in [(True, f'{DOMAIN}:3200'), (False, '192.168.50.67:3200'),
                      (False, '112.5.69.112:3200'), (False, f'{DOMAIN}:3200')]:
        for path, expected_port in [('/readyz', 8080), ('/api/webhooks/vcs/test', 8080),
                                  ('/login?next=%2Fsaveone', 3000), ('/api/me', 3000),
                                  ('/api/agent-task-snapshot', 3000)]:
            result = request(path, tls=tls, host=host, extra='Cookie: multica_auth=fixture\r\n')
            body = json.loads(result[2])
            assert result[0] == 200 and body['path'] == path and body['port'] == expected_port, result
            assert 'Location' not in result[1], 'Session requests must stay on their original origin'
            assert body['cookie'] == 'multica_auth=fixture', body
            assert body['host'] == host and body['scheme'] == ('https' if tls else 'http'), body
            assert '127.0.0.1' not in body['forwarded_for'], body
            check_cookies(result, tls)
    assert request('/readyz', tls=False, port=80)[0] == 200
    traefik = request('/readyz', tls=False, port=80, extra='X-Forwarded-Proto: https\r\n')
    assert json.loads(traefik[2])['scheme'] == 'https'
    check_cookies(traefik, True)
    spoofed = request('/readyz', tls=False, extra='X-Forwarded-Proto: https\r\n')
    assert json.loads(spoofed[2])['scheme'] == 'http'
    check_cookies(spoofed, False)
    print('PASS: HTTP and trusted HTTPS share port 3200; routes, host, client IP and scheme preserved', flush=True)
    print('PASS: LAN cookies work over HTTP, HTTPS cookies stay Secure, private Traefik HTTP 80 preserved', flush=True)

    for tls in [False, True]:
        handshake = request('/ws', tls=tls, extra='Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\n'
                          'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n')
        assert handshake[0] == 101, handshake
        assert handshake[1]['Sec-WebSocket-Accept'] == 's3pPLMBiTxaQ9kYGzzhZRbK+xOo=', handshake
    print('PASS: both plain and TLS WebSocket upgrade handshakes succeed on port 3200', flush=True)

    shutil.copy('/fixtures/leaf2.key', '/fixtures/tls/privkey.pem')
    time.sleep(3)
    assert request('/readyz')[3] == first[3], 'Mismatched renewal replaced the loaded certificate'
    print('PASS: a partially written/mismatched renewed certificate keeps the current TLS service working', flush=True)

    shutil.copy('/fixtures/chain2.pem', '/fixtures/tls/fullchain.next')
    os.replace('/fixtures/tls/fullchain.next', '/fixtures/tls/fullchain.pem')
    expected = hashlib.sha256(ssl.PEM_cert_to_DER_cert(Path('/fixtures/leaf2.crt').read_text())).hexdigest()
    for _ in range(20):
        result = request('/readyz')
        if result[3] == expected:
            break
        time.sleep(0.5)
    assert result[3] == expected and result[0] == 200, 'Renewed certificate did not become active'
    print('PASS: certificate replacement is detected and reloaded without a container restart', flush=True)


def outside():
    root = Path(__file__).resolve().parent
    suffix = f'{os.getpid()}-{int(time.time())}'
    network = f'multica-https-test-{suffix}'
    image = f'multica-https-test:{suffix}'
    gateway = f'multica-https-gateway-{suffix}'
    backend = f'multica-https-backend-{suffix}'
    with tempfile.TemporaryDirectory(prefix='multica-https-test-') as temp:
        fixture = Path(temp)
        env = dict(os.environ, POSTGRES_PASSWORD='fixture', JWT_SECRET='fixture',
                   MULTICA_LARK_SECRET_KEY='fixture', MULTICA_VCS_SECRET_KEY='fixture',
                   MULTICA_LAN_IP='192.168.50.67')
        config = json.loads(run('docker', 'compose', '--env-file', '/dev/null', '-f',
                                str(root / 'docker-compose.dokploy-https.yml'), 'config', '--format', 'json',
                                env=env, capture_output=True).stdout)
        service = config['services']['multica-gateway']
        assert service['ports'][0]['target'] == 3200 and str(service['ports'][0]['published']) == '3200'
        assert service['volumes'][0]['read_only'] and not service['volumes'][0]['bind']['create_host_path']
        for name, path in [('multica_nginx', 'nginx.conf'), ('multica_cert_reload', 'reload.sh')]:
            (fixture / path).write_text(config['configs'][name]['content'].replace('$$', '$'))
        run('/bin/sh', '-n', str(fixture / 'reload.sh'))

        # Deploy the actual Compose configs, including its final dollar-sign expansion.
        runtime = dict(services={'gateway': dict(
            image=service['image'], command=service['command'], container_name=gateway,
            environment={'MULTICA_CERT_RELOAD_INTERVAL': '1'}, configs=service['configs'],
            volumes=[dict(type='bind', source=str(fixture / 'tls'), target='/etc/nginx/tls',
                          read_only=True, bind={'create_host_path': False})],
            networks={'fixture': {'aliases': ['gateway']}})}, configs=config['configs'],
            networks={'fixture': {'external': True, 'name': network}})
        manifest = fixture / 'compose.json'
        manifest.write_text(json.dumps(runtime))
        compose = ['docker', 'compose', '-p', network, '-f', str(manifest)]

        with open(os.devnull, 'w') as quiet:
            run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '3',
                '-subj', '/CN=Multica Test CA', '-keyout', str(fixture / 'ca.key'),
                '-out', str(fixture / 'ca.crt'), stdout=quiet, stderr=quiet)
            (fixture / 'extensions').write_text(f'subjectAltName=DNS:{DOMAIN}\nextendedKeyUsage=serverAuth\n')
            for n in [1, 2]:
                run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', f'/CN={DOMAIN}',
                    '-keyout', str(fixture / f'leaf{n}.key'), '-out', str(fixture / f'leaf{n}.csr'),
                    stdout=quiet, stderr=quiet)
                run('openssl', 'x509', '-req', '-in', str(fixture / f'leaf{n}.csr'), '-CA', str(fixture / 'ca.crt'),
                    '-CAkey', str(fixture / 'ca.key'), '-set_serial', str(n), '-days', '2',
                    '-extfile', str(fixture / 'extensions'), '-out', str(fixture / f'leaf{n}.crt'),
                    stdout=quiet, stderr=quiet)
                (fixture / f'chain{n}.pem').write_text((fixture / f'leaf{n}.crt').read_text() +
                                                      (fixture / 'ca.crt').read_text())
        (fixture / 'tls').mkdir(mode=0o700)
        shutil.copy(fixture / 'chain1.pem', fixture / 'tls/fullchain.pem')
        shutil.copy(fixture / 'leaf1.key', fixture / 'tls/privkey.pem')
        for cert in (fixture / 'tls').iterdir():
            cert.chmod(0o600)
        dockerfile = '''FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash curl ca-certificates openssl python3 python3-yaml util-linux coreutils && rm -rf /var/lib/apt/lists/*
'''
        try:
            run('docker', 'build', '-t', image, '-', input=dockerfile)
            run('docker', 'network', 'create', network, stdout=subprocess.DEVNULL)
            run('docker', 'run', '-d', '--name', backend, '--network', network,
                '--network-alias', 'multica-api', '--network-alias', 'multica-web',
                '-v', f'{fixture}:/fixtures', '-v', f'{root}:/workspace:ro', image,
                'python3', '/workspace/docker-compose.dokploy-https.test.py', '--serve', stdout=subprocess.DEVNULL)
            for _ in range(30):
                if (fixture / 'backend-ready').exists():
                    break
                time.sleep(0.1)
            assert (fixture / 'backend-ready').exists()
            run(*compose, 'up', '-d', '--no-build', stdout=subprocess.DEVNULL)
            start = run('docker', 'inspect', gateway, '--format', '{{.State.StartedAt}}', capture_output=True).stdout
            run('docker', 'exec', backend, 'python3', '/workspace/docker-compose.dokploy-https.test.py', '--check')
            assert start == run('docker', 'inspect', gateway, '--format', '{{.State.StartedAt}}', capture_output=True).stdout
        except Exception:
            subprocess.run(['docker', 'logs', '--tail', '40', gateway], check=False)
            raise
        finally:
            subprocess.run([*compose, 'down'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'rm', '-f', backend, gateway], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'image', 'rm', image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    if sys.argv[1:] == ['--serve']:
        serve()
    elif sys.argv[1:] == ['--check']:
        check()
    else:
        outside()
