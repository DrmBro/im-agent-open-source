"""按原收件绑定，以个人身份发送群消息和原生 @。默认仅预览。"""
import argparse
import hashlib
import json
import sqlite3
import subprocess
import sys

from pending_messages import call as assistant_call
from read_context import dws_binary


def run(profile, args, body=None):
    cmd = [dws_binary(), '--profile', profile, '--format', 'json', *args]
    result = subprocess.run(cmd, input=body, text=True, capture_output=True, timeout=85)
    if result.returncode:
        raise ValueError('DWS 调用失败，请核对登录、权限和投递记录')
    value = json.loads(result.stdout)
    if 'outcome' in value:
        if value.get('outcome') != 'success' or not value.get('ok'):
            raise ValueError('DWS 未确认成功')
        if not value.get('dry_run'):
            return value['data']
    return value


def target(args):
    if args.group:
        if not args.profile or not args.group.startswith('cid'):
            raise ValueError('指定目标需要账号配置名和群 openConversationId')
        return args.profile, args.group, None
    quote = assistant_call({'action':'group_quote', 'id':args.event or '', 'thread':args.thread or ''}).get('quote') or {}
    if not quote.get('profile') or not quote.get('group') or not quote.get('message_id') or type(quote.get('recalled')) is not bool:
        raise ValueError('无法确认原群消息和撤回状态')
    return quote['profile'], quote['group'], quote


def mentions(profile, group, names):
    if not names:
        return []
    page = run(profile, ['chat', '+chat-members-list', '--conversation-id', group, '--member-types', 'user'])
    if page.get('conversationId') != group or page.get('complete') is not True or page.get('failedCount') or page.get('hasMore'):
        raise ValueError('群成员名单不完整，无法核对 @ 对象')
    ids = []
    for name in names:
        matches = {m['openDingtalkId'] for m in page['users'] if m.get('openDingtalkId') and
                   name in [m.get(k) for k in ('openDingtalkId','name','nick','memberNick','memberEmpName','memberGroupNick')]}
        if not matches:
            raise ValueError(f'目标群没有找到成员「{name}」')
        if len(matches) > 1:
            raise ValueError(f'群内「{name}」重名，请指定 openDingtalkId：'+ '、'.join(sorted(matches)))
        uid = matches.pop()
        if uid.lower()=='all' or any(ch.isspace() or ch in '<>,@' for ch in uid):
            raise ValueError('群成员标识无效')
        if uid not in ids:
            ids.append(uid)
    return ids


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    origin = parser.add_mutually_exclusive_group(required=True)
    for field in ('event','thread','group'):
        origin.add_argument('--'+field)
    parser.add_argument('--profile')
    parser.add_argument('--at', action='append', default=[], help='群成员姓名或 openDingtalkId，可重复')
    parser.add_argument('--send', action='store_true', help='账号主人明确要求发送后执行')
    parser.add_argument('--request-id', help='需要区分相同正文的独立发送意图时提供；同次重试保持一致')
    args = parser.parse_args()
    body = sys.stdin.read(262145).strip()
    if not body or '<@' in body or len(body.encode()) > 262144:
        raise ValueError('正文不能为空或超过 256 KiB；请通过 --at 指定提醒对象')
    profile, group, quote = target(args)
    ids = mentions(profile, group, args.at)
    text = ' '.join([*('<@'+uid+'>' for uid in ids), body])
    if len(text.encode()) > 262144:
        raise ValueError('含 @ 提醒的正文超过 256 KiB')
    key_data = [profile, group, sorted(ids), body, args.request_id or args.event or args.thread or '']
    if quote:
        key_data.append(quote['message_id'])
    key = hashlib.sha256(json.dumps(key_data,ensure_ascii=False).encode()).hexdigest()
    if quote and not quote['recalled']:
        flags = ['chat','+messages-reply','--as','user','--group',group,'--message-id',quote['message_id'],'--content','-']
    else:
        flags = ['chat','+messages-send','--identity','user','--group',group,'--text','-']
    flags.extend(['--body-stdin','--ai-tag=true','--idempotency-key',key,'--wait-delivery'])
    if ids:
        flags.extend(['--at-open-dingtalk-ids',','.join(ids)])
    try:
        receipt = run(profile,flags+(['--yes'] if args.send else ['--dry-run']),text)
    except (ValueError, OSError, subprocess.TimeoutExpired) as err:
        if args.send:
            raise ValueError(f'投递未确认，请先查原群，勿自动重发。幂等键 {key}') from err
        raise
    if not args.send:
        print(json.dumps({'status':'preview','group':group,'members':args.at,'mentions':ids,'quote':quote,'text':text,'key':key},ensure_ascii=False))
    elif receipt.get('deliveryStatus')=='delivered' and receipt.get('conversationId')==group and receipt.get('idempotencyKey')==key and (receipt.get('openMessageId') or (quote and not quote['recalled'] and receipt.get('messageId'))):
        print(json.dumps({'status':'delivered','message_id':receipt.get('openMessageId') or receipt['messageId'],'group':group,'mentions':ids},ensure_ascii=False))
    else:
        raise ValueError(f'缺少匹配的送达回执，请核对原群，勿自动重发。幂等键 {key}')


if __name__=='__main__':
    try:
        main()
    except (ValueError,OSError,sqlite3.Error,subprocess.TimeoutExpired) as err:
        raise SystemExit(str(err))
