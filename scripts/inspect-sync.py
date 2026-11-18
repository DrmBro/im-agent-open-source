#!/usr/bin/env python3
"""只读汇总运行版本、会话扫描、投递回执与审批状态；不返回消息正文。"""
import argparse
import http.client
import json
from pathlib import Path
import socket
import sqlite3
import time

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--data-dir',type=Path,default=Path.home()/'Library/Application Support/IM Agent')
parser.add_argument('--thread',default='')
parser.add_argument('--output',type=Path)
args=parser.parse_args()
base=args.data_dir.expanduser().resolve()
class Local(http.client.HTTPConnection):
 def connect(self):
  self.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
  self.sock.settimeout(12)
  self.sock.connect(str(base/'control.sock'))
report={'observed_at':int(time.time())}
try:
 conn=Local('local');conn.request('GET','/status',headers={'Authorization':'Bearer '+(base/'control.token').read_text()})
 response=conn.getresponse();status=json.loads(response.read());conn.close()
 if response.status!=200:raise RuntimeError('后台控制接口返回异常')
 report.update(phase=status.get('phase'),build=status.get('build'),diagnostics=status.get('diagnostics'),connections=[{'connection':r.get('connection'),'phase':r.get('phase')} for r in status.get('connections',[])])
except (OSError,ValueError,RuntimeError,http.client.HTTPException) as error:report['control_error']=type(error).__name__
configdb=sqlite3.connect((base/'desktop.sqlite').as_uri()+'?mode=ro',uri=True,timeout=5)
config=json.loads(configdb.execute('SELECT body FROM desktop_config WHERE id=1').fetchone()[0]);configdb.close()
business=Path(config.get('state_dir') or base/'runtime').expanduser().resolve()/'sessions.db'
db=sqlite3.connect(business.as_uri()+'?mode=ro',uri=True,timeout=5);db.row_factory=sqlite3.Row
report['delivery_totals']=[dict(r) for r in db.execute('SELECT state,count(*) AS parts FROM live_mirror_parts GROUP BY state')]
report['approval_totals']=[dict(r) for r in db.execute('SELECT state,delivery_state,count(*) AS cards FROM desktop_approval_cards GROUP BY state,delivery_state')]
report['active_sessions']=[dict(r) for r in db.execute("SELECT thread_id FROM sessions WHERE status='active' AND managed_chat=1")]
if args.thread:
 report['messages']=[dict(r) for r in db.execute('SELECT e.item_id,e.role,p.part_index,p.state,p.error,p.platform_id,p.next_attempt,p.updated_at FROM live_mirror_entries e JOIN live_mirror_parts p ON p.thread_id=e.thread_id AND p.item_id=e.item_id WHERE e.thread_id=? ORDER BY e.seq DESC,p.part_index LIMIT 100',(args.thread,))]
 report['read_issues']=[dict(r) for r in db.execute('SELECT byte_offset,kind,created_at FROM live_mirror_read_issues WHERE thread_id=? ORDER BY byte_offset DESC LIMIT 100',(args.thread,))]
 report['approval_cards']=[dict(r) for r in db.execute('SELECT state,delivery_state,decision,detail,message_id,updated_at FROM desktop_approval_cards WHERE thread_id=? ORDER BY created_at DESC LIMIT 20',(args.thread,))]
db.close()
installed=base/'installed-build.json'
if installed.exists():
 manifest=json.loads(installed.read_text())
 report['installed_source_digest']=manifest.get('source_digest')
 report['running_matches_installed']=report.get('build',{}).get('source_digest')==manifest.get('source_digest') if report.get('build') else False
text=json.dumps(report,ensure_ascii=False,indent=2)
if args.output:
 args.output.parent.mkdir(parents=True,exist_ok=True)
 args.output.write_text(text+'\n');args.output.chmod(0o600)
 print(json.dumps({'report':str(args.output),'phase':report.get('phase'),'active_sessions':len(report['active_sessions'])},ensure_ascii=False))
else:print(text)
