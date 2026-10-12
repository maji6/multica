#!/usr/bin/env python3
"""Run the installer against fake DNS/ACME and real Traefik TLS in isolated Docker containers.

No host Docker socket, production credentials, public ports or ACME CA are used.
Run: python3 scripts/setup-dokploy-spaceship.test.py
"""

import http.server
import json
import os
from pathlib import Path
import pty
import select
import shutil
import subprocess
import sys
import tempfile
import threading
import time


SCRIPT = "setup-dokploy-spaceship.sh"


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def write(path, text, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)


def interactive(secret, expected=0):
    pid, fd = pty.fork()
    if pid == 0:
        os.execv("/bin/bash", ["bash", f"/workspace/{SCRIPT}"])
    pending = [("邮箱：", "test@example.com"), ("Spaceship API Key：", "fixture-key"),
               ("Spaceship API Secret：", secret)]
    data = bytearray()
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        if select.select([fd], [], [], 0.2)[0]:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            if not chunk:
                break
            data.extend(chunk)
            if pending and pending[0][0].encode() in data:
                _, answer = pending.pop(0)
                os.write(fd, (answer + "\n").encode())
    else:
        os.kill(pid, 15)
        raise AssertionError("Installer timed out")
    _, status = os.waitpid(pid, 0)
    os.close(fd)
    output = data.decode(errors="replace")
    assert secret not in output and "fixture-key" not in output, "Credentials leaked into terminal output"
    code = os.waitstatus_to_exitcode(status)
    assert (code == 0) == (expected == 0), output
    return output


def inside():
    fixture = Path("/fixtures")
    state = Path("/opt/multica-acme")
    dynamic = fixture / "dynamic"
    for path in ["/run/systemd/system", "/etc/cron.d", "/fixtures/bin"]:
        Path(path).mkdir(parents=True, exist_ok=True)
    os.environ["PATH"] = "/fixtures/bin:" + os.environ["PATH"]
    # Only API requests and the installer download are simulated; HTTPS probes use real curl.
    write("/fixtures/bin/curl", """#!/bin/bash
set -eu
case "$*" in
  *raw.githubusercontent.com*)
    while [[ $# -gt 0 ]]; do
      if [[ $1 == -o ]]; then cp /fixtures/acme-installer "$2"; exit; fi
      shift
    done ;;
  *spaceship.dev*) cat /fixtures/api-status ;;
  *) exec /usr/bin/curl "$@" ;;
esac
""", 0o755)
    write("/fixtures/bin/apt-get", "#!/bin/sh\nexit 0\n", 0o755)
    write("/fixtures/bin/systemctl", "#!/bin/sh\nprintf '%s\\n' \"$*\" >>/fixtures/systemctl-calls\n", 0o755)
    write("/fixtures/bin/sleep", "#!/bin/sh\n[ -e /fixtures/fast-failure ] || exec /bin/sleep \"$@\"\n", 0o755)
    write("/fixtures/bin/docker", """#!/bin/sh
case "$*" in
  *Mounts*) cat /fixtures/mounts.json ;;
  *PortBindings*) printf '{"443/tcp":[{"HostIp":"%s","HostPort":"443"}]}' "$PROBE_IP" ;;
  *State.Running*) echo true ;;
  *Config.User*) echo ;;
  *Config.Labels*) cat /fixtures/labels.json ;;
  'ps -q --filter label=com.docker.compose.service=multica-gateway') echo fixture-gateway ;;
  *) echo "Unexpected docker action: $*" >&2; exit 1 ;;
esac
""", 0o755)
    write("/fixtures/acme-installer", """#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  if [ "$1" = --home ]; then shift; acme_home=$1; fi
  shift
done
mkdir -p "$acme_home/dnsapi"
cp /fixtures/mock-acme "$acme_home/acme.sh"
chmod 700 "$acme_home/acme.sh"
touch "$acme_home/dnsapi/dns_spaceship.sh" "$acme_home/account.conf"
""")
    write("/fixtures/mock-acme", """#!/bin/bash
set -euo pipefail
mode=$1
shift
printf '%s\\n' "$mode" >> /fixtures/acme-calls
while [[ $# -gt 0 ]]; do
  case $1 in
    --home) acme_home=$2; shift ;;
    --key-file) key=$2; shift ;;
    --fullchain-file) chain=$2; shift ;;
    --reloadcmd) reload=$2; shift ;;
  esac
  shift
done
case $mode in
  --register-account) exit ;;
  --issue)
    if [[ -e /fixtures/issued ]]; then exit 2; fi
    touch /fixtures/issued ;;
  --install-cert)
    generation=$(cat /fixtures/generation)
    cp "/fixtures/leaf${generation}.key" "$key"
    cp "/fixtures/chain${generation}.pem" "$chain"
    printf '%s' "$reload" > /fixtures/reload-command
    bash -c "$reload" ;;
  --cron)
    [[ $SPACESHIP_API_SECRET == fixture-rotated-secret ]]
    cp /fixtures/leaf2.key /opt/multica-acme/incoming/privkey.pem
    cp /fixtures/chain2.pem /opt/multica-acme/incoming/fullchain.pem
    bash -c "$(cat /fixtures/reload-command)" ;;
  *) exit 1 ;;
esac
""", 0o700)
    write("/fixtures/api-status", "200")
    write("/fixtures/generation", "1")
    write("/fixtures/labels.json", json.dumps({
        "traefik.http.routers.multica-websecure.rule": "Host(`multica.savetest.top`)",
        "traefik.http.routers.multica-websecure.entrypoints": "websecure",
    }))
    original_route = (dynamic / "existing.yml").read_bytes()
    original_static = (fixture / "traefik.yml").read_bytes()

    with open(os.devnull, "w") as quiet:
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3",
            "-subj", "/CN=Multica Fixture CA", "-keyout", "/fixtures/ca.key", "-out", "/fixtures/ca.crt",
            stdout=quiet, stderr=quiet)
        shutil.copy("/fixtures/ca.crt", "/usr/local/share/ca-certificates/multica-fixture.crt")
        run("update-ca-certificates", stdout=quiet, stderr=quiet)
        for n in [1, 2, 3]:
            run("openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=multica.savetest.top",
                "-keyout", f"/fixtures/leaf{n}.key", "-out", f"/fixtures/leaf{n}.csr", stdout=quiet, stderr=quiet)
            write("/fixtures/extensions", "subjectAltName=DNS:multica.savetest.top\nextendedKeyUsage=serverAuth\n")
            run("openssl", "x509", "-req", "-in", f"/fixtures/leaf{n}.csr", "-CA", "/fixtures/ca.crt",
                "-CAkey", "/fixtures/ca.key", "-set_serial", str(n), "-days", "2", "-extfile", "/fixtures/extensions",
                "-out", f"/fixtures/leaf{n}.crt", stdout=quiet, stderr=quiet)
            write(f"/fixtures/chain{n}.pem", Path(f"/fixtures/leaf{n}.crt").read_text() + Path("/fixtures/ca.crt").read_text())

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(503 if Path('/fixtures/backend-fail').exists() else 200)
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')

        def log_message(self, *_):
            pass

    server = http.server.HTTPServer(("0.0.0.0", 8080), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    interactive("fixture-initial-secret")
    print("PASS: complete installation with three prompts; real trusted TLS and /readyz", flush=True)
    assert (state / "dns.env").stat().st_mode & 0o777 == 0o600
    cron = Path('/etc/cron.d/multica-spaceship-cert')
    assert cron.stat().st_mode & 0o777 == 0o644
    assert cron.read_text().count('--renew') == 1
    config = dynamic / 'multica-spaceship-tls.yml'
    first = config.read_bytes()

    interactive("fixture-rotated-secret")
    assert config.read_bytes() == first
    assert cron.read_text().count('--renew') == 1
    assert 'fixture-rotated-secret' in (state / 'dns.env').read_text()
    run('/bin/bash', str(state / SCRIPT), '--renew')
    renewed = config.read_bytes()
    assert renewed != first
    print("PASS: repeated install, skipped issuance, credential rotation and renewal hot reload", flush=True)

    write('/fixtures/api-status', '403')
    calls = Path('/fixtures/acme-calls').read_bytes()
    interactive('fixture-denied-secret', expected=1)
    assert config.read_bytes() == renewed
    assert calls == Path('/fixtures/acme-calls').read_bytes()
    print("PASS: rejected API credentials do not issue or replace certificates", flush=True)

    shutil.copy('/fixtures/leaf1.key', state / 'incoming/privkey.pem')
    result = subprocess.run(['/bin/bash', str(state / SCRIPT), '--deploy'], capture_output=True, text=True)
    assert result.returncode != 0 and config.read_bytes() == renewed
    print("PASS: mismatched private key rejected before changing Traefik", flush=True)

    shutil.copy('/fixtures/leaf3.key', state / 'incoming/privkey.pem')
    shutil.copy('/fixtures/chain3.pem', state / 'incoming/fullchain.pem')
    write('/fixtures/backend-fail', '')
    write('/fixtures/fast-failure', '')
    result = subprocess.run(['/bin/bash', str(state / SCRIPT), '--deploy'], capture_output=True, text=True)
    assert result.returncode != 0 and config.read_bytes() == renewed, result.stdout + result.stderr
    Path('/fixtures/backend-fail').unlink()
    Path('/fixtures/fast-failure').unlink()
    for _ in range(10):
        recovered = subprocess.run(['/bin/bash', '-c',
            f'source /workspace/{SCRIPT}; load_config; '
            'probe_certificate "$(certificate_fingerprint /fixtures/chain2.pem)" /fixtures/recovered.json'],
            capture_output=True, text=True)
        if recovered.returncode == 0:
            break
        time.sleep(1)
    assert recovered.returncode == 0, recovered.stderr
    print("PASS: failed health verification restores the previous certificate configuration", flush=True)

    write(config, '# user-owned\ntls: {}\n')
    result = subprocess.run(['/bin/bash', str(state / SCRIPT), '--deploy'], capture_output=True, text=True)
    assert result.returncode != 0 and config.read_text() == '# user-owned\ntls: {}\n'
    assert (dynamic / 'existing.yml').read_bytes() == original_route
    assert (fixture / 'traefik.yml').read_bytes() == original_static
    print("PASS: user-owned configuration and existing routes remain unchanged", flush=True)
    server.shutdown()


def outside():
    root = Path(__file__).resolve().parent
    suffix = f'{os.getpid()}-{int(time.time())}'
    network = f'multica-acme-test-{suffix}'
    image = f'multica-acme-test:{suffix}'
    traefik = f'multica-acme-traefik-{suffix}'
    backend = f'multica-acme-backend-{suffix}'
    with tempfile.TemporaryDirectory(prefix='multica-acme-test-') as tmp:
        dynamic = Path(tmp) / 'dynamic'
        dynamic.mkdir()
        static = Path(tmp) / 'traefik.yml'
        write(static, '''providers:
  file:
    directory: /etc/dokploy/traefik/dynamic
    watch: true
entryPoints:
  web:
    address: ":80"
  websecure:
    address: ":443"
certificatesResolvers:
  letsencrypt:
    acme:
      httpChallenge:
        entryPoint: web
      caServer: http://127.0.0.1:9
''', 0o644)
        write(dynamic / 'existing.yml', '''http:
  routers:
    multica:
      rule: Host(`multica.savetest.top`)
      entryPoints: [websecure]
      service: backend
      tls:
        certResolver: letsencrypt
  services:
    backend:
      loadBalancer:
        servers:
          - url: http://backend:8080
''', 0o644)
        dockerfile = '''FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash curl ca-certificates openssl python3 python3-yaml util-linux coreutils && rm -rf /var/lib/apt/lists/*
'''
        try:
            run('docker', 'build', '-t', image, '-', input=dockerfile)
            run('docker', 'network', 'create', network, stdout=subprocess.DEVNULL)
            run('docker', 'run', '-d', '--name', traefik, '--network', network,
                '-v', f'{static}:/etc/traefik/traefik.yml:ro',
                '-v', f'{dynamic}:/etc/dokploy/traefik/dynamic:ro', 'traefik:v3.5',
                stdout=subprocess.DEVNULL)
            mounts = json.loads(run('docker', 'inspect', traefik, '--format', '{{json .Mounts}}',
                                    capture_output=True).stdout)
            # Inspect the real split mounts; translate only the host fixture root for the test container.
            for mount in mounts:
                mount['Source'] = str(Path('/fixtures') / Path(mount['Source']).relative_to(tmp))
            write(Path(tmp) / 'mounts.json', json.dumps(mounts))
            ip = run('docker', 'inspect', traefik, '--format', '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}',
                     capture_output=True).stdout.strip()
            run('docker', 'run', '--name', backend, '--network', network, '--network-alias', 'backend',
                '-e', f'PROBE_IP={ip}', '-v', f'{tmp}:/fixtures', '-v', f'{root}:/workspace:ro',
                image, 'python3', '/workspace/setup-dokploy-spaceship.test.py', '--inside')
        finally:
            subprocess.run(['docker', 'rm', '-f', backend, traefik], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'image', 'rm', image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    if sys.argv[1:] == ['--inside']:
        inside()
    else:
        outside()
