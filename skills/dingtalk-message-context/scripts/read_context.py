"""只读查询当前任务来源的钉钉消息；不落盘消息正文。"""
import argparse
import json
from pathlib import Path
import shutil
import sqlite3
import subprocess
from datetime import datetime, timedelta, timezone

SHANGHAI = timezone(timedelta(hours=8))


def readonly(path):
    return sqlite3.connect(path.resolve().as_uri() + '?mode=ro', uri=True, timeout=5)


def bound_sources(event=None, thread=None):
    base = Path.home() / 'Library/Application Support/IM Agent'
    with readonly(base / 'desktop.sqlite') as db:
        config = json.loads(db.execute('SELECT body FROM desktop_config WHERE id=1').fetchone()[0])
    state = Path(config.get('state_dir') or base / 'runtime')
    with readonly(state / 'sessions.db') as db:
        rows = db.execute("""SELECT p.chat,p.is_direct,p.user_id,p.received_at,p.payload
            FROM personal_inbox p LEFT JOIN inbound_messages i ON i.event_id=p.id
            WHERE p.channel='dingtalk_personal' AND """ +
            ('p.id=?' if event else 'i.thread_id=?') + ' ORDER BY p.received_at DESC',
            (event or thread,)).fetchall()
    if not rows:
        raise ValueError('未找到本机钉钉来源，请核对收件 ID 或提供目标对话')
    out = []
    for chat, direct, user, received, payload in rows:
        profile, marker, target = chat[len('account:'):].rpartition(':peer:' if direct else ':group:')
        if not chat.startswith('account:') or not profile or not marker or not target or (direct and user != target):
            raise ValueError('无法核对来源对话')
        out.append((profile, None if direct else target, target if direct else None,
                    received, json.loads(payload).get('ConversationID')))
    return out


def dws_binary():
    return shutil.which('dws') or str(Path.home()/'.local/bin/dws')


def sources(args):
    if args.group or args.peer:
        if not args.profile:
            raise ValueError('请指定来源账号 --profile')
        return [(args.profile, args.group, args.peer, args.time, None)]
    out = []
    for profile, group, peer, received, conversation in bound_sources(args.event, args.thread):
        moment = datetime.fromtimestamp(received, SHANGHAI) + timedelta(seconds=1 if args.direction=='older' else -1)
        out.append((profile, group, peer, args.time or moment.strftime('%Y-%m-%d %H:%M:%S'), conversation))
    return list(dict.fromkeys(out))


def excerpt(m):
    return {key: m[key] for key in ('messageId','openMessageId','createTime','senderNick','senderName','text')
            if m.get(key) and isinstance(m[key], (str, int))}


def read(args):
    binary = dws_binary()
    seen = set()
    for profile, group, peer, at, conversation in sources(args):
        cmd = [binary,'--profile',profile,'--format','json','chat','message','list',
               '--conversation-id' if group else '--open-dingtalk-id',group or peer,
               '--direction',args.direction,'--limit',str(args.limit)]
        if at:
            cmd += ['--time',at]
        result = subprocess.run(cmd,capture_output=True,timeout=35)
        if result.returncode:
            raise ValueError('钉钉读取失败，请核对该账号登录状态及对话读取权限')
        page = json.loads(result.stdout)
        if 'outcome' in page:
            if not page.get('ok') or page.get('outcome')!='success':
                raise ValueError('钉钉未确认查询成功')
            page = page['data']
        if not page.get('success') or not isinstance(page.get('messages'),list):
            raise ValueError('钉钉未返回有效的消息页')
        for m in page['messages']:
            expected = group or conversation
            if expected and any(m.get(k) not in (None,'',expected) for k in ('openConversationId','conversationId')):
                continue
            item = excerpt(m)
            key = (profile,group or peer,item.get('messageId') or item.get('openMessageId'))
            if not key[-1] or key in seen:
                continue
            seen.add(key)
            q=m.get('quotedMessage')
            if isinstance(q,dict) and all(not q.get(k) or q[k]==(expected or m.get(k)) for k in ('openConversationId','conversationId')):
                item['quotedMessage']=excerpt(q)
            print(json.dumps({'conversation':group or peer,**item},ensure_ascii=False))
    if not seen:
        print('此范围内没有可核对的消息。')


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    origin=parser.add_mutually_exclusive_group(required=True)
    for name in ('event','thread','group','peer'):
        origin.add_argument('--'+name)
    parser.add_argument('--profile')
    parser.add_argument('--direction',choices=('older','newer'),default='older')
    parser.add_argument('--time')
    parser.add_argument('--limit',type=int,choices=range(1,51),default=16)
    try:
        read(parser.parse_args())
    except (ValueError,OSError,sqlite3.Error,subprocess.TimeoutExpired) as err:
        raise SystemExit(str(err))
