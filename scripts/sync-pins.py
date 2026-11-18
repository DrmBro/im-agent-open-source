#!/usr/bin/env python3
"""将 Codex 置顶状态单向同步为飞书即时提醒；默认只读预览。"""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = Path.home() / 'Library/Application Support/IM Agent'
STATE_DIR = BASE / 'pin-sync'
STATE_PATH = STATE_DIR / 'state.json'


def read_db(path, query, parameters=()):
    connection = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=1)
    connection.row_factory = sqlite3.Row
    deadline = time.monotonic() + 3
    connection.set_progress_handler(lambda: int(time.monotonic() > deadline), 1000)
    try:
        return [dict(row) for row in connection.execute(query, parameters)]
    finally:
        connection.close()


def save(state):
    temporary = STATE_PATH.with_suffix('.tmp')
    with temporary.open('w') as file:
        json.dump(state, file, ensure_ascii=False, indent=2)
        file.flush()
        os.fsync(file.fileno())
    os.replace(temporary, STATE_PATH)


def snapshot():
    config = json.loads(read_db(BASE / 'desktop.sqlite', 'SELECT body FROM desktop_config')[0]['body'])
    settings = config.get('codex', {})
    home = (Path(settings['home']).expanduser() if settings.get('mode') == 'custom' and settings.get('home')
            else Path.home() / '.codex')
    source = home / '.codex-global-state.json'
    before = source.stat()
    value = json.loads(source.read_text())
    after = source.stat()
    if (before.st_ino, before.st_mtime_ns, before.st_size) != (after.st_ino, after.st_mtime_ns, after.st_size):
        raise RuntimeError('读取期间 Codex 置顶文件变化，等待下一轮')
    pins = value.get('pinned-thread-ids')
    if not isinstance(pins, list) or any(not isinstance(item, str) or not item for item in pins):
        raise RuntimeError('Codex 置顶列表字段缺失或格式变化，停止本轮同步')
    connections = {item['id']: item for item in config['connections'] if item['type'] == 'feishu'}
    sessions = read_db(BASE / 'runtime/sessions.db', """SELECT thread_id, connection_id, chat_id, owner_id
        FROM sessions WHERE status='active' AND managed_chat=1 AND channel='feishu'
        AND coalesce(topic_id,'')=''""")
    desired = {}
    for session in sessions:
        conn = connections.get(session['connection_id'])
        if not conn or session['owner_id'] not in conn.get('allowed_users', []):
            continue
        if not session['chat_id'].startswith('oc_') or not session['owner_id'].startswith('ou_'):
            continue
        if conn.get('shared_chat_id') == session['chat_id']:
            continue
        key = hashlib.sha256(json.dumps([session['connection_id'], session['chat_id'],
                                        session['owner_id']]).encode()).hexdigest()
        entry = desired.setdefault(key, dict(session, pinned=False))
        entry['pinned'] = entry['pinned'] or session['thread_id'] in pins
    return connections, desired, {'source': str(source), 'pinned_threads': len(pins),
                                  'bound_pinned': sum(item['pinned'] for item in desired.values())}


class FeishuError(RuntimeError):
    pass


def api(method, path, token=None, body=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    request = urllib.request.Request('https://open.feishu.cn/open-apis' + path,
                                     method=method, headers=headers,
                                     data=json.dumps(body).encode() if body is not None else None)
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            result = json.load(response)
    except urllib.error.HTTPError as error:
        try:
            result = json.loads(error.read())
        except Exception:
            raise FeishuError('飞书 HTTP ' + str(error.code)) from None
        raise FeishuError('飞书 HTTP ' + str(error.code) + ' code=' + str(result.get('code'))
                          + ' ' + str(result.get('msg', ''))[:1200]) from None
    if result.get('code') != 0:
        # 不记录原始请求、密钥或 token；错误消息通常包含所需权限名。
        raise FeishuError('飞书 code=' + str(result.get('code')) + ' ' + str(result.get('msg', ''))[:1200])
    return result


def token_for(connection_id):
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    account = read_db(BASE / 'desktop.sqlite', "SELECT value FROM desktop_meta WHERE key='vault_account'")[0]['value']
    key = subprocess.run(['/usr/bin/security', 'find-generic-password', '-s', 'io.im-agent.desktop',
                          '-a', account, '-w'], capture_output=True, check=True, timeout=10).stdout.strip()
    if key.startswith(b'go-keyring-base64:'):
        key = base64.b64decode(key.split(b':', 1)[1])
    elif key.startswith(b'go-keyring-encoded:'):
        key = bytes.fromhex(key.split(b':', 1)[1].decode())
    cipher = AESGCM(base64.b64decode(key))
    secrets = {}
    for row in read_db(BASE / 'desktop.sqlite', """SELECT field,ciphertext FROM desktop_credentials
            WHERE connection_id=? AND field IN ('app_id','app_secret')""", (connection_id,)):
        raw = row['ciphertext']
        scope = ('im-agent:v1:' + connection_id + ':' + row['field']).encode()
        secrets[row['field']] = cipher.decrypt(raw[:12], raw[12:], scope).decode()
    return api('POST', '/auth/v3/tenant_access_token/internal', body=secrets)['tenant_access_token']


def run(state):
    now = int(time.time())
    state['checked_at'] = now
    if (BASE / 'service.intent').read_text().strip() == 'stopped':
        state['phase'] = 'paused'
        save(state)
        return
    connections, desired, info = snapshot()
    state.update(info, phase='running', error='')
    tracked = state['tracked']
    # 只取得待处理项目所需的 token，每连接每轮最多获取一次。
    tokens = {}
    connection_errors = {}
    for key in sorted(set(desired) | set(tracked)):
        wanted = desired.get(key)
        entry = tracked.get(key)
        target = bool(wanted and wanted['pinned'])
        if entry is None:
            if not target:
                continue
            entry = dict(wanted, applied=None, pending=True, retry_at=0)
            tracked[key] = entry
        connection_id = entry['connection_id']
        conn = connections.get(connection_id)
        if not conn or entry['owner_id'] not in conn.get('allowed_users', []):
            entry['error'] = '连接或用户授权已移除，保留待处理状态'
            continue
        if not entry.get('pending') and entry.get('applied') == target:
            continue
        if now < entry.get('retry_at', 0):
            continue
        entry.update(pending=True, desired=target)
        save(state)  # 请求超时或进程退出后仍可撤销可能已生效的提醒。
        try:
            if connection_id in connection_errors:
                raise FeishuError(connection_errors[connection_id])
            if connection_id not in tokens:
                tokens[connection_id] = token_for(connection_id)
            result = api('PATCH', '/im/v2/feed_cards/' + urllib.parse.quote(entry['chat_id'], safe='')
                         + '?user_id_type=open_id', tokens[connection_id],
                         {'time_sensitive': target, 'user_ids': [entry['owner_id']]})
            failed = result.get('data', {}).get('failed_user_reasons', [])
            if failed:
                raise FeishuError('飞书用户级失败：' + json.dumps(failed, ensure_ascii=False)[:1200])
            entry.update(applied=target, pending=False, retry_at=0, error='', applied_at=int(time.time()))
            if not target:
                del tracked[key]
        except Exception as error:
            message = str(error) if isinstance(error, FeishuError) else '调用失败：' + type(error).__name__
            entry.update(error=message, retry_at=now + 300)
            connection_errors[connection_id] = message
        save(state)
    state['pending'] = sum(bool(item.get('pending')) for item in tracked.values())
    state['applied_pins'] = sum(item.get('applied') is True for item in tracked.values())
    state['phase'] = 'degraded' if any(item.get('error') for item in tracked.values()) else 'running'
    save(state)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='应用变化；默认仅预览')
    args = parser.parse_args()
    if not args.apply:
        _, desired, info = snapshot()
        print(json.dumps(dict(info, targets=[item for item in desired.values() if item['pinned']]),
                         ensure_ascii=False, indent=2))
        return
    os.umask(0o077)
    STATE_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (STATE_DIR / 'sync.lock').open('a+') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        state = json.loads(STATE_PATH.read_text()) if STATE_PATH.exists() else {'version': 1, 'tracked': {}}
        if state.get('version') != 1 or not isinstance(state.get('tracked'), dict):
            raise RuntimeError('本地置顶同步状态格式异常，停止同步')
        try:
            run(state)
        except Exception as error:
            state.update(phase='degraded', error='读取或保存失败：' + type(error).__name__)
            save(state)
        print(json.dumps({key: state.get(key) for key in
                          ('checked_at', 'phase', 'bound_pinned', 'applied_pins', 'pending', 'error')},
                         ensure_ascii=False))


if __name__ == '__main__':
    main()
