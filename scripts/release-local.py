#!/usr/bin/env python3
"""从完整工作区生成可追溯的本机发布，校验后备份安装并恢复后台。"""
import argparse
import datetime
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import plistlib
import shutil
import socket
import sqlite3
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent

def run(args, **kwargs):
    result = subprocess.run(args, text=True, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError(Path(str(args[0])).name + " 执行失败：" + result.stderr[-3000:])
    return result.stdout.strip()

def digest(path):
    result = hashlib.sha256()
    with path.open('rb') as file:
        for block in iter(lambda: file.read(1024 * 1024), b''):
            result.update(block)
    return result.hexdigest()

def source_digest(files):
    return hashlib.sha256(json.dumps(files, ensure_ascii=False, sort_keys=True, separators=(',', ':')).encode()).hexdigest()

def save_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    path.chmod(0o600)

def collect_source():
    paths = run(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=ROOT).split('\0')
    files = {}
    for name in sorted(set(paths)):
        if not name:
            continue
        path = ROOT / name
        if path.is_symlink():
            raise RuntimeError('发布输入包含符号链接：' + name)
        if path.is_file():
            files[name] = digest(path)
    for required in ['go.mod', 'main.go', 'frontend/app.js', 'internal/codex/desktop_adapter.js']:
        if required not in files:
            raise RuntimeError('完整发布缺少源文件：' + required)
    return files

def build(args):
    files = collect_source()
    source_hash = source_digest(files)
    now = datetime.datetime.now(datetime.timezone.utc)
    built_at = now.strftime('%Y-%m-%dT%H:%M:%SZ')
    revision = run(['git', 'rev-parse', '--short=12', 'HEAD'], cwd=ROOT)
    if run(['git', 'status', '--porcelain'], cwd=ROOT):
        revision += '-working'
    release = ROOT / '.work/releases' / ('unified-' + now.strftime('%Y%m%dT%H%M%SZ') + '-' + source_hash[:12])
    source = release / 'source'
    source.mkdir(parents=True, mode=0o700)
    for name in files:
        target = source / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(ROOT / name, target)
        if digest(target) != files[name]:
            raise RuntimeError('快照过程中源文件发生变化：' + name)
    executable = release / 'im-agent'
    env = os.environ.copy()
    env.update(GOTOOLCHAIN='local', CGO_ENABLED='1', CGO_LDFLAGS='-framework UniformTypeIdentifiers -mmacosx-version-min=15.0')
    flags = ' '.join('-X=im-agent/internal/buildinfo.' + key + '=' + value for key, value in {
        'SourceDigest': source_hash, 'Revision': revision, 'BuiltAt': built_at}.items())
    run([args.go, 'build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-tags', 'desktop,wv2runtime.download,production', '-ldflags', flags, '-o', str(executable), '.'], cwd=source, env=env)
    run([args.go, 'build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags', flags, '-o', str(release / 'sync-inspect'), './cmd/sync-inspect'], cwd=source, env=env)
    bundle = release / 'IM Agent.app'
    (bundle / 'Contents/MacOS').mkdir(parents=True)
    (bundle / 'Contents/Resources').mkdir()
    template = Path(args.application)
    run(['codesign', '--verify', '--deep', '--strict', str(template)])
    info = plistlib.loads(run(['plutil', '-convert', 'xml1', '-o', '-', str(template / 'Contents/Info.plist')]).encode())
    if info.get('CFBundleExecutable') != 'im-agent':
        raise RuntimeError('应用模板可执行文件不匹配')
    info.update(IMAgentSourceDigest=source_hash, IMAgentRevision=revision, IMAgentBuiltAt=built_at, LSMinimumSystemVersion='15.0')
    (bundle / 'Contents/Info.plist').write_bytes(plistlib.dumps(info))
    icon = template / 'Contents/Resources/iconfile.icns'
    if icon.is_file():
        shutil.copy2(icon, bundle / 'Contents/Resources/iconfile.icns')
    notices = source / 'third_party/happy'
    (bundle / 'Contents/Resources/HAPPY_NOTICE.txt').write_text((notices / 'NOTICE.md').read_text() + '\n' + (notices / 'LICENSE').read_text())
    shutil.copy2(executable, bundle / 'Contents/MacOS/im-agent')
    run(['codesign', '--force', '--deep', '--sign', '-', str(bundle)])
    run(['codesign', '--verify', '--deep', '--strict', str(bundle)])
    manifest = {'source_digest': source_hash, 'revision': revision, 'built_at': built_at, 'files': files,
                'binary_sha256': digest(bundle / 'Contents/MacOS/im-agent'), 'inspector_sha256': digest(release / 'sync-inspect'), 'verification': {'tests_run': False}}
    save_json(release / 'manifest.json', manifest)
    verify_release(release)
    print(json.dumps({'release': str(release), 'source_digest': source_hash, 'revision': revision}, ensure_ascii=False), flush=True)
    return release

def verify_release(release):
    manifest = json.loads((release / 'manifest.json').read_text())
    if source_digest(manifest['files']) != manifest['source_digest']:
        raise RuntimeError('源码清单摘要不匹配')
    for name, expected in manifest['files'].items():
        path = Path(name)
        if path.is_absolute() or '..' in path.parts or digest(release / 'source' / name) != expected:
            raise RuntimeError('发布源码校验失败')
    bundle = release / 'IM Agent.app'
    if digest(bundle / 'Contents/MacOS/im-agent') != manifest['binary_sha256']:
        raise RuntimeError('发布程序校验失败')
    if digest(release / 'sync-inspect') != manifest['inspector_sha256']:
        raise RuntimeError('诊断程序校验失败')
    run(['codesign', '--verify', '--deep', '--strict', str(bundle)])
    return manifest

def readonly(path):
    return sqlite3.connect(path.resolve().as_uri() + '?mode=ro', uri=True, timeout=5)

def wait(predicate, seconds=40):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.4)
    raise RuntimeError('等待后台状态超时')

def deploy(args, release):
    manifest = verify_release(release)
    if source_digest(collect_source()) != manifest['source_digest']:
        raise RuntimeError('工作区在构建后已变化，需要从完整源码重新构建')
    base = Path(args.data_dir).expanduser().resolve()
    application = Path(args.application)
    stable = base / 'bin/im-agent-service'
    domain = 'gui/' + str(os.getuid())
    label = domain + '/io.im-agent.background'
    agent = Path.home() / 'Library/LaunchAgents/io.im-agent.background.plist'
    launch = plistlib.loads(agent.read_bytes())
    if launch.get('ProgramArguments', [None])[0] != str(stable):
        raise RuntimeError('后台启动配置不匹配，未替换程序')
    db = readonly(base / 'desktop.sqlite')
    config = json.loads(db.execute('SELECT body FROM desktop_config WHERE id=1').fetchone()[0])
    db.close()
    business = Path(config.get('state_dir') or base / 'runtime').expanduser().resolve() / 'sessions.db'
    class Local(http.client.HTTPConnection):
        def connect(self):
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.settimeout(self.timeout)
            self.sock.connect(str(base / 'control.sock'))
    def call(method, path, timeout=12):
        conn = Local('local', timeout=timeout)
        try:
            conn.request(method, path, body='{}' if method == 'POST' else None, headers={
                'Authorization': 'Bearer ' + (base / 'control.token').read_text(), 'Content-Type': 'application/json'})
            response = conn.getresponse()
            value = json.loads(response.read())
            if response.status != 200:
                raise RuntimeError('后台控制失败：HTTP ' + str(response.status))
            return value
        finally:
            conn.close()
    def available():
        try:
            return call('GET', '/status').get('phase') in ('running', 'starting', 'stopped', 'failed')
        except (OSError, http.client.HTTPException):
            return False
    def lock_free():
        with (base / 'daemon.lock').open('r+') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return False
            fcntl.flock(lock, fcntl.LOCK_UN)
            return True
    def idle():
        db = readonly(business)
        try:
            # desktop-* 只停止日志观察，真实桌面轮次保持运行；原生执行中的任务必须等待。
            active = db.execute("SELECT count(*) FROM events e WHERE e.status='processing' AND e.stage<>'queued' AND NOT EXISTS(SELECT 1 FROM request_results r WHERE r.event_id=e.id AND r.turn_ref LIKE 'desktop-%' AND r.execution_state='submitted')").fetchone()[0]
            tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            states = {'live_mirror_parts': "state='sending'", 'session_status_cards': "state='sending'", 'session_presentation': "history_state='sending'", 'desktop_approval_cards': "delivery_state='sending' OR state='responding'", 'attachment_transfers': "state IN ('sending','downloading')"}
            return active == 0 and not any(db.execute('SELECT count(*) FROM ' + table + ' WHERE ' + where).fetchone()[0] for table, where in states.items() if table in tables)
        finally:
            db.close()
    def backup_database(path, target):
        src, dst = readonly(path), sqlite3.connect(target)
        try:
            src.backup(dst)
        finally:
            src.close(); dst.close()
        target.chmod(0o600)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:6]
    backup = base / 'backups' / ('unified-' + stamp)
    staged = application.parent / ('.IM-Agent-updating-' + stamp + '.app')
    unloaded = swapped = False
    was_running = False
    with (base / 'maintenance.lock').open('a+') as maintenance:
        fcntl.flock(maintenance, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            was_running = call('GET', '/status').get('phase') in ('running', 'starting')
            wait(idle)
            backup.mkdir(parents=True, mode=0o700)
            shutil.copy2(stable, backup / 'im-agent-service')
            shutil.copy2(base / 'service.intent', backup / 'service.intent')
            shutil.copy2(release / 'manifest.json', backup / 'installed-manifest.json')
            run(['ditto', str(release / 'IM Agent.app'), str(staged)])
            run(['codesign', '--verify', '--deep', '--strict', str(staged)])
            print('已完成发布校验并准备备份：' + str(backup), flush=True)
            call('POST', '/stop', 30)
            wait(lambda: call('GET', '/status').get('phase') in ('stopped', 'failed'))
            run(['launchctl', 'bootout', label]); unloaded = True
            wait(lock_free)
            backup_database(base / 'desktop.sqlite', backup / 'desktop.sqlite')
            backup_database(business, backup / 'sessions.db')
            os.rename(application, backup / 'IM Agent.app')
            try:
                os.rename(staged, application)
            except BaseException:
                os.rename(backup / 'IM Agent.app', application)
                raise
            swapped = True
            temporary = stable.with_name('.service-updating-' + stamp)
            shutil.copy2(application / 'Contents/MacOS/im-agent', temporary)
            temporary.chmod(0o700)
            os.replace(temporary, stable)
            if digest(stable) != manifest['binary_sha256']:
                raise RuntimeError('安装后的后台摘要不一致')
            run(['launchctl', 'bootstrap', domain, str(agent)]); unloaded = False
            wait(available)
            if was_running:
                call('POST', '/start', 45)
            def healthy():
                status = call('GET', '/status')
                correct = status.get('build', {}).get('source_digest') == manifest['source_digest']
                connections = status.get('connections', [])
                return correct and (status.get('phase') == 'running' and bool(connections) and all(row.get('phase') == 'connected' for row in connections) if was_running else status.get('phase') == 'stopped')
            wait(healthy, 45)
            save_json(base / 'installed-build.json', manifest)
            save_json(release / 'deployment.json', {'backup': str(backup), 'source_digest': manifest['source_digest'], 'phase': call('GET', '/status').get('phase')})
            print('DEPLOY_OK ' + json.dumps({'source_digest': manifest['source_digest'], 'backup': str(backup), 'phase': call('GET', '/status').get('phase')}, ensure_ascii=False), flush=True)
        except BaseException:
            if swapped:
                try:
                    if not unloaded:
                        call('POST', '/stop', 30)
                        wait(lambda: call('GET', '/status').get('phase') in ('stopped', 'failed'))
                        run(['launchctl', 'bootout', label]); unloaded = True
                    wait(lock_free)
                    failed = application.parent / ('.IM-Agent-failed-' + stamp + '.app')
                    os.rename(application, failed)
                    os.rename(backup / 'IM Agent.app', application)
                    shutil.copy2(backup / 'im-agent-service', stable)
                    print('已回退旧程序；升级后的投递账本保留。', flush=True)
                except BaseException as error:
                    print('程序回退需要核对：' + str(error), flush=True)
            if unloaded:
                try:
                    run(['launchctl', 'bootstrap', domain, str(agent)])
                    wait(available)
                except BaseException:
                    print('后台重新加载需要核对。', flush=True)
            if was_running:
                try:
                    if call('GET', '/status').get('phase') in ('stopped', 'failed'):
                        call('POST', '/start', 45)
                except BaseException:
                    print('旧后台连接恢复需要核对。', flush=True)
            raise

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['build', 'deploy', 'verify'])
    parser.add_argument('--release', type=Path)
    parser.add_argument('--go', default=shutil.which('go') or 'go')
    parser.add_argument('--application', default='/Applications/IM Agent.app')
    parser.add_argument('--data-dir', default=str(Path.home() / 'Library/Application Support/IM Agent'))
    args = parser.parse_args()
    if args.action == 'build':
        build(args)
    elif not args.release:
        parser.error('--release 是必填项')
    elif args.action == 'deploy':
        deploy(args, args.release.resolve())
    else:
        manifest = verify_release(args.release.resolve())
        print(json.dumps({'verified': True, 'source_digest': manifest['source_digest']}))
if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('发布未完成：' + str(error), file=sys.stderr)
        sys.exit(1)
