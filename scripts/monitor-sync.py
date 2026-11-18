#!/usr/bin/env python3
"""本机只读同步巡检；状态改变时通知，正常及重复异常保持安静。"""
import datetime
import fcntl
import http.client
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import time

BASE = Path.home() / 'Library/Application Support/IM Agent'
MONITOR = BASE / 'monitor'


def atomic_json(path, value):
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    temporary.chmod(0o600)
    os.replace(temporary, path)


def status():
    connection = http.client.HTTPConnection('localhost', timeout=8)
    connection.sock = socket.socket(socket.AF_UNIX)
    connection.sock.settimeout(8)
    try:
        connection.sock.connect(str(BASE / 'control.sock'))
        connection.request('GET', '/status', headers={
            'Authorization': 'Bearer ' + (BASE / 'control.token').read_text().strip()})
        response = connection.getresponse()
        if response.status != 200:
            raise RuntimeError('控制接口不可用')
        return json.loads(response.read())
    finally:
        connection.close()


def inspect(previous):
    now = int(time.time())
    report = {'checked_at': now, 'issues': {}, 'phase': 'unknown'}
    issues = report['issues']
    try:
        running = status()
        report['phase'] = running.get('phase', 'unknown')
    except Exception:
        issues['control_unavailable'] = '后台控制接口不可访问'
        running = {}
    intent = (BASE / 'service.intent').read_text().strip()
    if intent == 'stopped':
        report['paused'] = True
        issues.clear()
        return report
    if running and report['phase'] != 'running':
        issues['service_not_running'] = '后台未正常运行'
    try:
        connection = sqlite3.connect((BASE / 'runtime/sessions.db').as_uri() + '?mode=ro',
                                     uri=True, timeout=1)
        try:
            deadline = time.monotonic() + 3
            connection.set_progress_handler(lambda: int(time.monotonic() > deadline), 1000)
            sessions = connection.execute("""SELECT s.thread_id,
              coalesce(h.read_checked_at,0),coalesce(h.delivery_checked_at,0),
              coalesce(h.read_error,''),coalesce(h.delivery_error,''),
              coalesce(c.checked_at,0),coalesce(c.error,'')
              FROM sessions s LEFT JOIN session_sync_health h ON h.thread_id=s.thread_id
              LEFT JOIN inbound_history_cursors c ON c.thread_id=s.thread_id
              WHERE s.status='active' AND s.managed_chat=1""").fetchall()
            report['active_sessions'] = len(sessions)
            if any(now - min(r[1], r[2]) > 180 for r in sessions):
                issues['sync_stale'] = '会话同步超过 3 分钟未推进检查时间'
            if any(r[3] or r[4] for r in sessions):
                issues['sync_error'] = '会话读取或投递发生错误'
            if any(now - r[5] > 180 or r[6] for r in sessions):
                issues['intake_error'] = '收件对账失败或超过 3 分钟未更新'
            unknown = connection.execute("""SELECT count(*) FROM live_mirror_parts p
              JOIN sessions s ON s.thread_id=p.thread_id
              WHERE s.status='active' AND p.state='unknown'""").fetchone()[0]
            report['unknown'] = unknown
            baseline = previous.get('unknown_baseline', unknown)
            report['unknown_baseline'] = baseline
            if unknown > baseline:
                issues['new_unknown_delivery'] = '出现新增的送达不确定片段'
            old_pending = connection.execute("""SELECT count(*) FROM live_mirror_parts p
              JOIN sessions s ON s.thread_id=p.thread_id WHERE s.status='active'
              AND p.state IN ('pending','sending') AND p.updated_at<?""", (now - 600,)).fetchone()[0]
            if old_pending:
                issues['delivery_stalled'] = '存在超过 10 分钟未完成的消息投递'
            rows = connection.execute("""SELECT f.thread_id,f.path,f.byte_offset
              FROM live_mirror_file_cursors f JOIN sessions s ON s.thread_id=f.thread_id
              WHERE s.status='active' AND s.managed_chat=1""").fetchall()
        finally:
            connection.close()
        backlog = {}
        for thread, path, offset in rows:
            try:
                size = Path(path).stat().st_size
            except OSError:
                continue  # 移动或归档由同步器诊断，不据缺失直接报警。
            if size > offset:
                key = thread + ':' + path
                backlog[key] = offset
                if previous.get('backlog', {}).get(key) == offset:
                    issues['read_stalled'] = '日志仍有未读内容，且两次巡检间游标没有推进'
        report['backlog'] = backlog
    except sqlite3.OperationalError as error:
        issues['database_locked' if 'locked' in str(error).lower() else 'database_unavailable'] = (
            '业务数据库被锁住' if 'locked' in str(error).lower() else '业务数据库读取失败或超时')
    except Exception:
        issues['inspection_failed'] = '巡检数据读取失败'
    return report


def notify(message):
    script = '''on run argv
display notification (item 1 of argv) with title "IM Agent 同步巡检"
end run'''
    result = subprocess.run(['/usr/bin/osascript', '-e', script, message],
                            capture_output=True, timeout=10)
    return result.returncode == 0


def main():
    MONITOR.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (MONITOR / 'check.lock').open('a+') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        state_path = MONITOR / 'state.json'
        previous = json.loads(state_path.read_text()) if state_path.exists() else {}
        try:
            report = inspect(previous)
        except Exception:
            report = {'checked_at': int(time.time()), 'issues': {'monitor_failed': '巡检运行失败'}}
        counts = {code: previous.get('counts', {}).get(code, 0) + 1 for code in report['issues']}
        # 短暂波动连续两次才报警；锁库和已经对比两轮的读取停滞立即报警。
        alerts = sorted(code for code, count in counts.items()
                        if count >= 2 or code in ('database_locked', 'read_stalled'))
        old_alerts = previous.get('notified_alerts', [])
        notification_ok = True
        if alerts != old_alerts and not report.get('paused'):
            message = ('；'.join(report['issues'][code] for code in alerts) if alerts
                       else '此前同步异常已恢复，定时巡检继续运行。')
            try:
                notification_ok = notify(message)
            except Exception:
                notification_ok = False
            event = {'at': datetime.datetime.now().astimezone().isoformat(),
                     'alerts': alerts, 'message': message, 'notification_submitted': notification_ok}
            log = MONITOR / 'alerts.jsonl'
            if log.exists() and log.stat().st_size > 1 << 20:
                os.replace(log, MONITOR / 'alerts.previous.jsonl')
            with log.open('a') as file:
                file.write(json.dumps(event, ensure_ascii=False) + '\n')
            log.chmod(0o600)
        state = dict(report, counts=counts,
                     notified_alerts=alerts if notification_ok else old_alerts)
        if 'unknown_baseline' not in state and 'unknown_baseline' in previous:
            state['unknown_baseline'] = previous['unknown_baseline']
        atomic_json(state_path, state)
        print(json.dumps({'checked_at': report['checked_at'], 'phase': report.get('phase'),
                          'active_sessions': report.get('active_sessions'),
                          'alerts': alerts, 'paused': report.get('paused', False)}, ensure_ascii=False))


if __name__ == '__main__':
    main()
