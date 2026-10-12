#!/usr/bin/env bash
# Install and renew a DNS-01 certificate for the existing Multica Dokploy route.
# Run on the Ubuntu/Debian Dokploy host: sudo bash setup-dokploy-spaceship.sh
# DNS credentials are read from the terminal, never from command-line arguments.
# References:
# https://github.com/acmesh-official/acme.sh/wiki/dnsapi2#182-use-spaceship-dns-api
# https://github.com/acmesh-official/acme.sh#3%EF%B8%8F%E2%83%A3-install-the-certificate-to-apachenginx

set +x
set -Eeuo pipefail
umask 077

DOMAIN=multica.savetest.top
TRAEFIK=dokploy-traefik
STATE=/opt/multica-acme
ACME_HOME=$STATE/acme.sh
INSTALLED_SCRIPT=$STATE/setup-dokploy-spaceship.sh
MARKER='# Managed by Multica Spaceship certificate installer v1'
STAGE=初始化
WORK_DIR=

say() { printf '\n%s\n' "$*"; }
fail() { printf '\n错误：%s\n' "$*" >&2; return 1; }

cleanup() {
  unset SPACESHIP_API_KEY SPACESHIP_API_SECRET
  if [[ -n $WORK_DIR && -d $WORK_DIR ]]; then rm -rf -- "$WORK_DIR"; fi
}

on_error() {
  local code=$?
  printf '\n未完成：%s（退出码 %s）。修复上方错误后可重新运行；不要公开 account.conf 或私钥。\n' "$STAGE" "$code" >&2
  exit "$code"
}

check_owned_file() {
  local file=$1
  [[ ! -L $file ]] || { fail "拒绝覆盖符号链接：$file"; return 1; }
  if [[ -e $file ]] && ! grep -Fxq "$MARKER" "$file"; then
    fail "已有非本脚本管理的文件：$file。请先检查，脚本不会覆盖它。"
    return 1
  fi
}

load_config() {
  [[ -f $STATE/paths.conf && ! -L $STATE/paths.conf ]] || { fail '找不到已安装的路径配置。'; return 1; }
  # This file is generated locally with shell-escaped values, inside a root-only directory.
  source "$STATE/paths.conf"
}

validate_certificate() {
  local chain=$STATE/incoming/fullchain.pem key=$STATE/incoming/privkey.pem
  openssl x509 -in "$chain" -noout -checkend 86400 >/dev/null || return 1
  openssl verify -verify_hostname "$DOMAIN" -untrusted "$chain" "$chain" >/dev/null || return 1
  local cert_public key_public
  cert_public=$(openssl x509 -in "$chain" -pubkey -noout) || return 1
  key_public=$(openssl pkey -in "$key" -pubout) || return 1
  [[ $cert_public == "$key_public" ]] || { fail '证书与私钥不匹配。'; return 1; }
}

certificate_fingerprint() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':'
}

probe_certificate() {
  local expected=$1 body=$2 served
  curl --noproxy '*' --silent --show-error --fail --connect-timeout 2 --max-time 5 \
    --resolve "$DOMAIN:$PROBE_PORT:$PROBE_IP" \
    "https://$DOMAIN:$PROBE_PORT/readyz" -o "$body" || return 1
  python3 - "$body" <<'PY' || return 1
import json, sys
with open(sys.argv[1]) as f:
    result = json.load(f)
if result.get('status') != 'ok':
    raise SystemExit('Multica /readyz did not return status: ok')
PY
  served=$(timeout 5 openssl s_client -connect "$PROBE_IP:$PROBE_PORT" -servername "$DOMAIN" \
    </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':') || return 1
  [[ $served == "$expected" ]]
}

deploy_certificate() {
  STAGE=加载并验证证书
  load_config
  exec 8>"$STATE/deploy.lock"
  flock -n 8 || { fail '已有证书加载操作正在运行。'; return 1; }
  validate_certificate || { fail '证书校验失败，现有 Traefik 配置未修改。'; return 1; }
  check_owned_file "$TLS_CONFIG" || return 1

  local fingerprint version_dir container_dir temp_config backup probe_dir success=0
  fingerprint=$(certificate_fingerprint "$STATE/incoming/fullchain.pem")
  [[ $fingerprint =~ ^[A-Fa-f0-9]{64}$ ]] || { fail '无法读取证书指纹。'; return 1; }
  version_dir=$HOST_DYNAMIC/certificates/multica-spaceship/$fingerprint
  container_dir=$CONTAINER_DYNAMIC/certificates/multica-spaceship/$fingerprint
  [[ ! -L $HOST_DYNAMIC/certificates && ! -L $HOST_DYNAMIC/certificates/multica-spaceship && ! -L $version_dir ]] || {
    fail '证书目录包含符号链接，已停止。'; return 1;
  }
  install -d -m 700 "$version_dir"
  install -m 600 "$STATE/incoming/fullchain.pem" "$version_dir/fullchain.pem"
  install -m 600 "$STATE/incoming/privkey.pem" "$version_dir/privkey.pem"

  probe_dir=$(mktemp -d "$STATE/probe.XXXXXX")
  backup=$probe_dir/previous.yml
  if [[ -f $TLS_CONFIG ]]; then cp -p "$TLS_CONFIG" "$backup"; fi
  # New certificate paths force a real configuration update in the file provider.
  # Only the final atomic rename is visible as a YAML configuration file.
  temp_config=$(mktemp "$HOST_DYNAMIC/.multica-spaceship.XXXXXX")
  {
    printf '%s\n' "$MARKER"
    printf 'tls:\n  certificates:\n    - certFile: "%s/fullchain.pem"\n      keyFile: "%s/privkey.pem"\n' "$container_dir" "$container_dir"
  } >"$temp_config"
  chmod 600 "$temp_config"
  mv -f "$temp_config" "$TLS_CONFIG"

  for ((attempt=0; attempt<12; attempt++)); do
    if probe_certificate "$fingerprint" "$probe_dir/body.json" >"$probe_dir/error.log" 2>&1; then
      success=1
      break
    fi
    sleep 2
  done
  if [[ $success == 0 ]]; then
    cat "$probe_dir/error.log" >&2
    # Keep the previous usable configuration, without touching other routes/certificates.
    if [[ -f $backup ]]; then
      mv -f "$backup" "$TLS_CONFIG"
    else
      mv "$TLS_CONFIG" "$probe_dir/failed-config.yml"
    fi
    fail "证书加载或 /readyz 验证失败，已恢复此前的证书配置。诊断文件：$probe_dir"
    return 1
  fi
  rm -rf -- "$probe_dir"
  say "证书已加载：$DOMAIN；可信 HTTPS、证书指纹和 /readyz 均通过。"
}

preflight_dokploy() {
  STAGE='检查 Dokploy'
  docker inspect "$TRAEFIK" --format '{{json .Mounts}}' >"$WORK_DIR/mounts.json"
  docker inspect "$TRAEFIK" --format '{{json .HostConfig.PortBindings}}' >"$WORK_DIR/ports.json"
  [[ $(docker inspect "$TRAEFIK" --format '{{.State.Running}}') == true ]] || { fail 'dokploy-traefik 未运行。'; return 1; }
  case $(docker inspect "$TRAEFIK" --format '{{.Config.User}}') in
    ''|root|0|0:0|root:root) ;;
    *) fail '当前脚本仅支持 Dokploy 默认的 root Traefik 容器。'; return 1 ;;
  esac
  python3 - "$WORK_DIR" >"$WORK_DIR/paths.conf" <<'PY'
import json, pathlib, shlex, sys, yaml
root = pathlib.Path(sys.argv[1])
mounts = json.loads((root / 'mounts.json').read_text())
def host_path(path):
    matches = [m for m in mounts if path == m['Destination'] or path.startswith(m['Destination'].rstrip('/') + '/')]
    if not matches:
        raise SystemExit(f'Traefik path is not mounted: {path}')
    m = max(matches, key=lambda x: len(x['Destination']))
    if m['Type'] != 'bind':
        raise SystemExit('Only the standard Dokploy bind mounts are supported')
    return str(pathlib.Path(m['Source']) / path[len(m['Destination']):].lstrip('/'))
# Dokploy mounts the host configuration at Traefik's standard container path.
static = yaml.safe_load(pathlib.Path(host_path('/etc/traefik/traefik.yml')).read_text())
provider = static.get('providers', {}).get('file', {})
dynamic = provider.get('directory')
if dynamic != '/etc/dokploy/traefik/dynamic' or provider.get('watch', True) is not True:
    raise SystemExit('Expected Dokploy dynamic directory with file watching enabled')
ports = json.loads((root / 'ports.json').read_text()).get('443/tcp') or []
ipv4 = [p for p in ports if ':' not in p.get('HostIp', '')]
if not ipv4:
    raise SystemExit('Traefik has no published IPv4 HTTPS port')
port = ipv4[0]
ip = port.get('HostIp') or '0.0.0.0'
ip = '127.0.0.1' if ip == '0.0.0.0' else ip
host = host_path(dynamic)
if not pathlib.Path(host).is_dir() or pathlib.Path(host).is_symlink():
    raise SystemExit('The dynamic host directory is missing or is a symlink')
values = dict(HOST_DYNAMIC=host, CONTAINER_DYNAMIC=dynamic, PROBE_IP=ip,
              PROBE_PORT=port['HostPort'], TLS_CONFIG=host + '/multica-spaceship-tls.yml')
for k, v in values.items():
    if any(c in str(v) for c in '\n\r"\\'):
        raise SystemExit('Unsupported characters in Docker paths')
    print(f'{k}={shlex.quote(str(v))}')
PY
  source "$WORK_DIR/paths.conf"
  check_owned_file "$TLS_CONFIG"
  local containers
  containers=$(docker ps -q --filter label=com.docker.compose.service=multica-gateway)
  [[ -n $containers ]] || { fail '未找到运行中的 multica-gateway，请先恢复原 Dokploy 应用。'; return 1; }
  local -a ids
  read -r -a ids <<<"${containers//$'\n'/ }"
  docker inspect --format '{{json .Config.Labels}}' "${ids[@]}" >"$WORK_DIR/labels.jsonl"
  python3 - "$WORK_DIR/labels.jsonl" "$DOMAIN" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    labels = json.loads(line) or {}
    for key, value in labels.items():
        if key.startswith('traefik.http.routers.') and key.endswith('.rule') and value == f'Host(`{sys.argv[2]}`)':
            router = key[:-5]
            if 'websecure' in labels.get(router + '.entrypoints', '').split(','):
                print('Existing Multica HTTPS router found')
                raise SystemExit(0)
raise SystemExit('No existing Multica HTTPS route found; configure multica-gateway:80 in Dokploy first')
PY
}

read_credentials() {
  [[ -r /dev/tty ]] || { fail '请在交互终端运行脚本。'; return 1; }
  read -r -p '邮箱：' ACME_EMAIL </dev/tty
  [[ $ACME_EMAIL =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || { fail '邮箱格式不正确。'; return 1; }
  read -r -s -p 'Spaceship API Key：' SPACESHIP_API_KEY </dev/tty
  printf '\n'
  read -r -s -p 'Spaceship API Secret：' SPACESHIP_API_SECRET </dev/tty
  printf '\n'
  [[ -n $SPACESHIP_API_KEY && -n $SPACESHIP_API_SECRET ]] || { fail 'API Key 和 Secret 不能为空。'; return 1; }
  [[ ! $SPACESHIP_API_KEY =~ [[:cntrl:]] && ! $SPACESHIP_API_SECRET =~ [[:cntrl:]] ]] || {
    fail 'API 凭据包含控制字符，请重新复制。'; return 1;
  }
}

save_credentials() {
  STAGE='检查 Spaceship API 凭据'
  local headers=$WORK_DIR/spaceship-headers status
  printf 'X-API-Key: %s\nX-API-Secret: %s\n' "$SPACESHIP_API_KEY" "$SPACESHIP_API_SECRET" >"$headers"
  status=$(curl --silent --show-error --connect-timeout 15 --max-time 45 \
    --header "@$headers" --output "$WORK_DIR/spaceship-response.json" --write-out '%{http_code}' \
    'https://spaceship.dev/api/v1/dns/records/savetest.top?take=1&skip=0')
  rm -f "$headers"
  [[ $status == 200 ]] || { fail "Spaceship API 返回 HTTP $status。检查 Key、Secret 和 DNS Records Read/Write 权限。"; return 1; }
  # Keep renewed credentials available even when --issue skips a still-valid certificate.
  # The values are escaped for Bash and never appear in argv, cron files or console output.
  {
    printf 'export SPACESHIP_API_KEY=%q\n' "$SPACESHIP_API_KEY"
    printf 'export SPACESHIP_API_SECRET=%q\n' "$SPACESHIP_API_SECRET"
    printf 'export SPACESHIP_ROOT_DOMAIN=savetest.top\n'
  } >"$WORK_DIR/dns.env"
}

install_acme() {
  STAGE='安装 acme.sh'
  if [[ ! -x $ACME_HOME/acme.sh ]]; then
    curl --fail --silent --show-error --location --retry 2 --connect-timeout 15 --max-time 120 \
      https://raw.githubusercontent.com/acmesh-official/acme.sh/master/acme.sh -o "$WORK_DIR/acme-installer.sh"
    # Use the upstream installer directly so installation failures keep their exit status.
    sh "$WORK_DIR/acme-installer.sh" --install-online --home "$ACME_HOME" --config-home "$ACME_HOME" \
      --accountemail "$ACME_EMAIL" --nocron
  fi
  [[ -x $ACME_HOME/acme.sh && -f $ACME_HOME/dnsapi/dns_spaceship.sh ]] || {
    fail 'acme.sh 安装不完整，或缺少 dns_spaceship 插件。'; return 1;
  }
  "$ACME_HOME/acme.sh" --register-account --home "$ACME_HOME" --server letsencrypt --accountemail "$ACME_EMAIL"
}

issue_certificate() {
  STAGE='Spaceship DNS 签发'
  export SPACESHIP_API_KEY SPACESHIP_API_SECRET
  export SPACESHIP_ROOT_DOMAIN=savetest.top
  local code=0
  "$ACME_HOME/acme.sh" --issue --home "$ACME_HOME" --server letsencrypt \
    --dns dns_spaceship -d "$DOMAIN" --keylength ec-256 || code=$?
  unset SPACESHIP_API_KEY SPACESHIP_API_SECRET
  # acme.sh returns 2 when an existing certificate is not due for renewal.
  [[ $code == 0 || $code == 2 ]] || { fail "DNS 签发失败（$code），尚未替换 Traefik 证书。"; return 1; }
  chmod 700 "$ACME_HOME"
  if [[ -f $ACME_HOME/account.conf ]]; then chmod 600 "$ACME_HOME/account.conf"; fi
  install -d -m 700 "$STATE/incoming"
  "$ACME_HOME/acme.sh" --install-cert --home "$ACME_HOME" -d "$DOMAIN" --ecc \
    --key-file "$STATE/incoming/privkey.pem" --fullchain-file "$STATE/incoming/fullchain.pem" \
    --reloadcmd "/bin/bash $INSTALLED_SCRIPT --deploy"
}

install_renewal() {
  STAGE=配置自动续期
  local cron=/etc/cron.d/multica-spaceship-cert
  check_owned_file "$cron"
  local temp
  temp=$(mktemp /etc/cron.d/.multica-spaceship.XXXXXX)
  {
    printf '%s\n' "$MARKER"
    printf 'SHELL=/bin/bash\nPATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n'
    printf '23 3 * * * root /bin/bash %s --renew >> %s/renew.log 2>&1\n' "$INSTALLED_SCRIPT" "$STATE"
  } >"$temp"
  chmod 644 "$temp"
  mv -f "$temp" "$cron"
  systemctl enable --now cron
  systemctl is-active --quiet cron
}

install_all() {
  STAGE=检查宿主机
  [[ -f /etc/debian_version && -d /run/systemd/system ]] || { fail '支持 Ubuntu/Debian 的 systemd 宿主机，请勿在应用容器或 Mac 上运行。'; return 1; }
  command -v docker >/dev/null || { fail '宿主机没有 Docker。'; return 1; }
  command -v flock >/dev/null || { fail '缺少 flock，请先安装 util-linux。'; return 1; }
  [[ ! -L $STATE ]] || { fail "$STATE 是符号链接，已停止。"; return 1; }
  if [[ -d $STATE && ! -f $STATE/.multica-managed ]]; then
    fail "$STATE 已存在但不属于此脚本，已停止以保留其内容。"
    return 1
  fi
  install -d -m 700 "$STATE"
  printf '%s\n' "$MARKER" >"$STATE/.multica-managed"
  exec 9>"$STATE/operation.lock"
  flock -n 9 || { fail '已有安装或续期任务正在运行。'; return 1; }
  WORK_DIR=$(mktemp -d "$STATE/setup.XXXXXX")
  check_owned_file /etc/cron.d/multica-spaceship-cert

  STAGE=安装依赖
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y curl ca-certificates openssl cron python3 python3-yaml
  preflight_dokploy
  read_credentials
  save_credentials
  install_acme
  install -m 600 "$WORK_DIR/paths.conf" "$STATE/paths.conf"
  if [[ $(readlink -f "${BASH_SOURCE[0]}") != "$INSTALLED_SCRIPT" ]]; then
    install -m 700 "${BASH_SOURCE[0]}" "$INSTALLED_SCRIPT"
  fi
  # The deployment subprocess inherits the install lock; it uses a separate lock.
  issue_certificate
  install -m 600 "$WORK_DIR/dns.env" "$STATE/dns.env"
  install_renewal
  say "完成：$DOMAIN 的可信 HTTPS 已验证，自动续期已启用。"
  printf '证书到期时间：'
  openssl x509 -in "$STATE/incoming/fullchain.pem" -noout -enddate
  printf '续期日志：%s/renew.log\n' "$STATE"
  printf '双协议 Compose 部署后，公网验证：关闭手机 Wi-Fi，访问 https://%s:3200/readyz\n' "$DOMAIN"
  printf '3200 入口使用已有端口映射；脚本仅验证本机 Traefik，未验证公网连通性。\n'
}

main() {
  case ${1:-install} in
    -h|--help)
      printf '用法：sudo bash %s\n仅输入邮箱、Spaceship API Key、Secret。目标：%s。\n' "$0" "$DOMAIN"
      printf '要求 Ubuntu/Debian systemd 宿主机，Dokploy 已有 multica-gateway:80 HTTPS 路由。\n'
      return ;;
    install|--deploy|--renew) ;;
    *) fail '未知参数。使用 --help 查看用法。'; return 1 ;;
  esac
  [[ $EUID == 0 && $(uname -s) == Linux ]] || { fail '请在 Linux 宿主机使用 sudo bash 运行。'; return 1; }
  trap cleanup EXIT
  trap on_error ERR
  case ${1:-install} in
    install) install_all ;;
    --deploy) deploy_certificate ;;
    --renew)
      STAGE=自动续期
      exec 9>"$STATE/operation.lock"
      flock -n 9 || return 0
      source "$STATE/dns.env"
      "$ACME_HOME/acme.sh" --cron --home "$ACME_HOME"
      ;;
  esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
