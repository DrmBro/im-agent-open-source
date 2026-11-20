/* 原生 Wails 绑定；无远程脚本、无前端依赖、无凭证 localStorage。 */
'use strict';
const $ = id => document.getElementById(id);
const settingsPages=new Set(['connections','codex','limits','watch']);
const titles = {overview:'概览',assistant:'消息助理',connections:'IM 连接',codex:'Codex 配置',limits:'运行限制',watch:'会话同步'};
let snapshot, draft, descriptors=[], status={}, localInfo=null, pending=new Map(), dirty=false, busy=false;
let sourceRefreshAt=0, eventRefreshAt=0;
let savedIDs=new Set(), page='overview', polling=false;
const connectionExpansion=new Map();
function updateConnectionStatuses(){
 const labels={connected:'已连接',connecting:'连接中',reconnecting:'重连中',disconnected:'已断开',failed:'异常',stopped:'已停止'};
 document.querySelectorAll('.connection-live-status').forEach(el=>{const current=(status.connections||[]).find(c=>c.connection===el.dataset.connection);el.textContent=current?(labels[current.phase]||'状态待确认'):'尚未连接';});
}

function api(){const value=window.go?.desktop?.API;if(!value)throw new Error('此页面需要在 IM Agent 桌面客户端中打开，不能直接用浏览器打开 HTML。');return value;}
function node(tag,cls,text){const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;}
let noteTimer=0;
// 成功提示数秒后自动收起；错误提示保留到用户关闭或下一条提示。
function note(text,error=false){
 const banner=$('banner');clearTimeout(noteTimer);
 banner.querySelector('.banner-text').textContent=String(text);banner.className=error?'error':'';banner.setAttribute('role',error?'alert':'status');banner.hidden=false;
 banner.style.animation='none';void banner.offsetWidth;banner.style.animation='';
 if(!error)noteTimer=setTimeout(()=>{banner.hidden=true;},7000);
}
function dismissNote(){clearTimeout(noteTimer);$('banner').hidden=true;}
$('banner').querySelector('.banner-close').onclick=dismissNote;
$('banner').addEventListener('mouseenter',()=>clearTimeout(noteTimer));
function modified(){dirty=true;api().SetDirty(true).catch(()=>{});updateHeader();}
function updateHeader(){
 $('dirty-badge').textContent=dirty?'有未保存修改':'已保存';document.title=(dirty?'• ':'')+'IM Agent';$('dirty-badge').className='badge'+(dirty?' unsaved':'');
 $('save').disabled=busy||!draft||!dirty;$('reload').disabled=busy;$('import').disabled=busy;
 $('add-connection').disabled=busy||!draft;
 updateStatus();
}
let busyTimer=0;
// 短操作只显示顶栏进度条；超过 400ms 才弹出“正在处理”，避免闪烁。
async function task(fn){if(busy)return;busy=true;document.body.classList.add('working');busyTimer=setTimeout(()=>{$('busy').hidden=false;},400);updateHeader();
 try{await fn();}catch(e){note(String(e?.message||e),true);}finally{busy=false;clearTimeout(busyTimer);document.body.classList.remove('working');$('busy').hidden=true;updateHeader();}
}
// danger=true 时以红色突出确认按钮，并默认聚焦“取消”，防止回车误触。
function confirmAction(title,message,{danger=false,confirmText='确认'}={}){return new Promise(resolve=>{
 $('confirm-title').textContent=title;$('confirm-message').textContent=message;$('confirm-yes').textContent=confirmText;
 const d=$('confirm-dialog');let finished=false;d.classList.toggle('danger',danger);
 const done=value=>{if(finished)return;finished=true;d.close();d.removeEventListener('cancel',cancel);d.removeEventListener('click',outside);resolve(value);};
 const cancel=e=>{e.preventDefault();done(false);};d.addEventListener('cancel',cancel);
 const outside=e=>{if(e.target===d){const r=d.getBoundingClientRect();if(e.clientX<r.left||e.clientX>r.right||e.clientY<r.top||e.clientY>r.bottom)done(false);}};d.addEventListener('click',outside);
 $('confirm-no').onclick=()=>done(false);$('confirm-yes').onclick=()=>done(true);d.showModal();(danger?$('confirm-no'):$('confirm-yes')).focus();
 });}
function navigate(p){if(p!=='assistant')window.assistantPage?.leave();const changed=page!==p;page=p;Object.keys(titles).forEach(key=>$('page-'+key).hidden=key!==p);document.querySelectorAll('.nav').forEach(n=>{const on=n.dataset.page===p;n.classList.toggle('active',on);if(on)n.setAttribute('aria-current','page');else n.removeAttribute('aria-current');});$('breadcrumb').textContent=titles[p];$('breadcrumb').previousSibling.textContent=(settingsPages.has(p)?'设置':'工作台')+' / ';if(changed)$('main').scrollTop=0;if(p==='assistant')window.assistantPage?.enter();if(p==='overview')refreshBackground().catch(()=>{});}
function bindValue(parent,label,value,set,{help='',type='text',options=null,pick=null,disabled=false}={}){
 const field=node('div','field'),lab=node('label','',label);const input=node(options?'select':type==='textarea'?'textarea':'input');
 const id='field-'+crypto.randomUUID();input.id=id;lab.htmlFor=id;field.append(lab);
 if(options){options.forEach(([v,t])=>{const o=node('option','',t);o.value=v;input.append(o);});}
 else if(type!=='textarea'){input.type=type;input.autocomplete='off';input.spellcheck=false;}
 input.value=value??'';input.disabled=disabled;
 input.addEventListener('input',()=>{set(type==='number'?Number(input.value):input.value);modified();});
 if(pick){const row=node('div','input-row');row.append(input);const button=node('button','btn','选择…');button.type='button';button.onclick=()=>task(async()=>{const value=await pick();if(value){input.value=value;set(value);modified();}});row.append(button);field.append(row);}else field.append(input);
 if(help)field.append(node('small','',help));parent.append(field);return input;
}
function toggle(parent,label,value,set,help=''){
 const field=node('div','field'),row=node('div','toggle-line'),input=node('input');input.type='checkbox';input.checked=!!value;input.id='toggle-'+crypto.randomUUID();const lab=node('label','',label);lab.htmlFor=input.id;input.onchange=()=>{set(input.checked);modified();};row.append(input,lab);field.append(row);if(help)field.append(node('small','',help));parent.append(field);
}
function listValue(value){return (value||'').split(/[\n,，]/).map(x=>x.trim()).filter(Boolean);}
function connectionDescriptor(c){return descriptors.find(d=>d.type===c.type);}
function applySnapshot(s){snapshot=s;draft=structuredClone(s.config);draft.connections ||= [];savedIDs=new Set(draft.connections.map(c=>c.id));pending.clear();dirty=false;api().SetDirty(false).catch(()=>{});render();}
function render(){renderConnections();renderCodex();renderLimits();renderWatch();renderOverview();updateHeader();}
function renderOverview(){
 $('connection-count').textContent=snapshot.config.connections?.length||0;$('revision').textContent=snapshot.revision||'—';$('saved-time').textContent=snapshot.updated_at?new Date(snapshot.updated_at).toLocaleString():'尚未保存';$('database-path').textContent=snapshot.database_path;$('state-path').textContent=snapshot.config.state_dir;
}
function updateStatus(){
 updateConnectionStatuses();
 const names={stopped:'已停止',starting:'启动中',running:'运行中',stopping:'停止中',failed:'启动或运行失败'};
 const phase=status.phase||'stopped';$('status-title').textContent=names[phase]||phase;
 $('status-dot').className='dot '+phase;$('hero').dataset.phase=phase;
 $('status-heading').textContent=phase==='running'?'你的桥接进程正在运行':phase==='starting'?'正在准备桥接连接':phase==='stopping'?'正在安全停止':phase==='failed'?'需要检查连接配置':'由你决定何时开始';
 $('status-message').textContent=status.message||'客户端不会自动启动桥接。';
 $('policy-notice').hidden=!status.policy_notice;$('policy-notice').textContent=status.policy_notice||'';
 if(phase==='running'){
  const connections=status.connections||[],scan=status.diagnostics||{};
  if(connections.some(c=>c.phase==='failed'||c.phase==='disconnected')){
   $('status-title').textContent='进程运行 · IM 连接异常';$('status-heading').textContent='IM 连接需要检查';$('status-dot').className='dot failed';
  }else if(scan.intake_phase==='degraded'){
   $('status-title').textContent='IM 已连接 · 收件对账异常';$('status-heading').textContent='收件对账需要检查';$('status-dot').className='dot failed';
  }else if(connections.length&&connections.every(c=>c.phase==='connected')){
   $('status-title').textContent='IM 已连接';$('status-heading').textContent='消息入口已连接';
  }else{$('status-title').textContent='进程运行 · 等待 IM 连接';$('status-heading').textContent='正在连接消息入口';}
 }

 const active=['starting','running','stopping'].includes(phase);
 $('start').disabled=busy||active||dirty||!snapshot?.revision||!snapshot.config.connections?.length;
 $('stop').disabled=busy||!active||phase==='stopping';
 const mismatch=active&&snapshot?.revision!==status.revision;
 const changed=active&&status.codex_changed;
 const messages=[];if(mismatch)messages.push(`已保存 v${snapshot.revision}；当前运行 v${status.revision}。停止后重新启动才会应用。`);
 if(changed)messages.push('检测到本机 Codex 配置或认证文件发生变化（可能由 CC Switch 切换）。当前任务不会自动中断，请停止后重新启动桥接。');
 $('restart-note').hidden=messages.length===0;$('restart-note').textContent=messages.join(' ');
 const indicator=$('codex-change');if(indicator){indicator.hidden=!changed;indicator.textContent=changed?messages[messages.length-1]:'';}

 const health=$('connection-health');if(health){health.replaceChildren();const names={waiting:'尚未连接',connecting:'连接中',connected:'已连接',reconnecting:'重连中',disconnected:'已断开',failed:'连接失败',stopped:'已停止'};
  for(const c of status.connections||[]){const row=node('div','connection-health');row.append(node('strong','',c.connection+' · '+(names[c.phase]||c.phase)),node('p','',c.message),node('p','muted',`收到文字消息 ${c.received} 条，其中白名单拒绝 ${c.rejected} 条`));health.append(row);}
 }
 const diag=status.diagnostics||{};
 $('message-health').textContent='消息处理：'+(diag.message||'尚未收到通过白名单的消息');
 const intake=$('intake-health');if(intake)intake.textContent='收件对账：'+(diag.intake||'尚未完成群历史对账；连接正常不代表消息已收到');
 const build=$('build-health');if(build)build.textContent='运行版本：'+(status.build?.revision||'未记录')+' · 源码摘要 '+(status.build?.source_digest||'未记录').slice(0,12);

}
function renderConnections(){
 const list=$('connection-list');list.replaceChildren();
 if(!draft.connections.length){const empty=node('div','panel empty');empty.append(node('div','symbol','▧'),node('h2','','添加第一个 IM 连接'),node('p','','凭证仅保存在本机，只有白名单用户可以使用机器人。'));const b=node('button','btn primary','＋ 添加连接');b.onclick=addConnection;empty.append(b);list.append(empty);return;}
 draft.connections.forEach(c=>{
  const d=connectionDescriptor(c);const shell=node('details','panel connection connection-fold'),panel=node('div','connection-fields'),head=node('summary','connection-header'),intro=node('div');shell.open=connectionExpansion.has(c.id)?connectionExpansion.get(c.id):!savedIDs.has(c.id);shell.ontoggle=()=>connectionExpansion.set(c.id,shell.open);intro.append(node('h3','',d?.name||c.type),node('p','',c.id||'新连接'));const remove=node('button','btn danger small','删除连接');remove.type='button';remove.onclick=()=>task(async()=>{if(!await confirmAction('删除连接？','保存后会删除该连接的本地加密凭证；业务历史保留。正在运行的连接不会立即停止。',{danger:true,confirmText:'删除'}))return;draft.connections=draft.connections.filter(x=>x!==c);if(draft.watch.connection_id===c.id){draft.watch.enabled=false;draft.watch.connection_id='';}for(const [k,v] of pending){if(v.connection_id===c.id)pending.delete(k);}modified();renderConnections();renderWatch();});const health=node('span','badge connection-live-status');health.dataset.connection=c.id;head.append(intro,health,node('span','muted','展开 / 收起'));shell.append(head,panel);panel.append(remove);
  const basic=node('div','two-cols');
  bindValue(basic,'连接 ID',c.id,value=>{const old=c.id;connectionExpansion.set(value,shell.open);if(old!==value)connectionExpansion.delete(old);c.id=value;health.dataset.connection=value;for(const [k,v] of [...pending]){if(v.connection_id===old){pending.delete(k);v.connection_id=value;pending.set(value+'\0'+v.field,v);}}head.querySelector('p').textContent=value;renderWatch();},{disabled:savedIDs.has(c.id),help:'唯一标识；保存后不可直接改名，避免凭证与会话关联错位。'});
  bindValue(basic,'渠道',c.type,value=>{c.type=value;if(value!=='dingtalk_personal'){c.personal_auto_reply_users=[];c.personal_all_direct=false;c.personal_mentions=false;c.personal_group_followup=false;c.personal_followup_minutes=0;}if(value==='dingtalk'||value==='dingtalk_personal'){c.auto_create_chat=false;c.shared_chat_id='';}if(!descriptors.find(x=>x.type===value)?.supports_topics)c.conversation_mode='group';for(const [k,v] of pending){if(v.connection_id===c.id)pending.delete(k);}renderConnections();},{disabled:savedIDs.has(c.id),options:descriptors.map(x=>[x.type,x.name])});panel.append(basic);
  (d?.credentials||[]).forEach(f=>{
   const field=node('div','field'),label=node('label','',f.label+(f.required?' *':'')),key=c.id+'\0'+f.key;
   const stored=snapshot.credentials?.[c.id]?.[f.key]||'missing';const state=node('span','secret-status '+stored,{saved:'已加密保存',missing:'未配置'}[stored]);label.append(state);field.append(label);
   const row=node('div','input-row'),input=node('input');input.id='secret-'+crypto.randomUUID();label.htmlFor=input.id;input.type='password';input.autocomplete='new-password';input.spellcheck=false;input.value=pending.get(key)?.value||'';input.placeholder=stored==='missing'?'输入后加密保存':'留空保留原值，不回显';
   const clear=node('button','btn small','清除本地值');clear.type='button';
   const sync=()=>{const p=pending.get(c.id+'\0'+f.key);state.textContent=p?.clear?'待清除本地值':p?.value?'待更新':{saved:'已加密保存',missing:'未配置'}[stored];clear.textContent=p?.clear?'撤销清除':'清除本地值';};
   input.oninput=()=>{const k=c.id+'\0'+f.key;if(input.value)pending.set(k,{connection_id:c.id,field:f.key,value:input.value,clear:false});else pending.delete(k);sync();modified();};
   clear.onclick=()=>{const k=c.id+'\0'+f.key;if(pending.get(k)?.clear)pending.delete(k);else pending.set(k,{connection_id:c.id,field:f.key,value:'',clear:true});input.value='';sync();modified();};row.append(input,clear);field.append(row,node('small','',f.help+' 仅使用客户端保存的凭证；清除后不会从环境变量补回。'));panel.append(field);sync();
  });
  const lists=node('div','two-cols');bindValue(lists,'用户白名单 *',(c.allowed_users||[]).join('\n'),v=>c.allowed_users=listValue(v),{type:'textarea',help:d?.user_hint});bindValue(lists,'管理员 *',(c.admin_users||[]).join('\n'),v=>c.admin_users=listValue(v),{type:'textarea',help:'管理员必须同时存在于用户白名单中。'});panel.append(lists);
  bindValue(panel,'群名标识（放在会话名称后）',c.chat_name_prefix,v=>c.chat_name_prefix=v);
  if(d?.supports_topics){
   bindValue(panel,'会话展示方式',c.conversation_mode||'group',v=>{c.conversation_mode=v;renderConnections();},{options:[['group','独立群：一个会话一个群（推荐）'],['topic','共享群话题：一个群集中多个会话']],help:'只影响新建或尚未绑定的会话。已绑定的旧群不会迁移、解散或重复发送历史。'});
   if(c.conversation_mode==='topic'){
    bindValue(panel,'共享群 ID（可选）',c.shared_chat_id||'',v=>c.shared_chat_id=v.trim(),{help:'填写机器人已加入的 oc_ 开头群 ID；留空会自动创建专用话题群。不要填写已绑定独立会话的旧群。'});
    panel.append(node('p','notice warning','共享群内的会话信息与历史可能被所有群成员看到。白名单只限制执行权限，不隔离阅读权限；请确认成员范围。自动创建时邀请管理员和首次发起人，其他使用者需加入该群。'));
    panel.append(node('p','muted','在机器人私聊或共享群发送 /new 创建话题，后续进入对应话题交流。归档只标记话题入口，不解散共享群。'));
   }
  }
  if(c.type==='dingtalk_personal'){toggle(panel,'接收全部个人单聊',!!c.personal_all_direct,v=>c.personal_all_direct=v,'开启后无需逐个添加联系人，消息默认进入人工审核。');toggle(panel,'接收群内 @我的消息',!!c.personal_mentions,v=>{c.personal_mentions=v;if(!v)c.personal_group_followup=false;},'只接收 @你的消息，不订阅群内所有消息；群回复必须人工确认。');panel.append(node('p','notice','个人消息默认先判断并生成草稿，通过桌面通知和任务看板等待本人审核，不直接回复。管理员只填本人 openDingtalkId，未开启全部单聊时，白名单填写本人和允许处理的联系人；开启后白名单仍用于指定自动回复规则。模型可调用现有工具处理业务，回复由本人审核。'));bindValue(panel,'允许自动回复的联系人（可选，默认全部待审核）',(c.personal_auto_reply_users||[]).join('\n'),v=>c.personal_auto_reply_users=listValue(v),{type:'textarea',help:'只填写白名单中希望自动回复的联系人 openDingtalkId，每行一个。仅对规则生效后新收到、模型明确判断能够处理的消息自动回复；需要补充信息或人工决定的消息仍等待审核。'});}else if(c.type==='dingtalk'){panel.append(node('p','notice','钉钉固定通过独立 Codex CLI 执行，不依赖桌面。私聊或已有群首次发送文本后绑定会话；不自动建群。支持 /new、/status、/stop、/respond、/archive。当前不支持图片、文件和互动卡片。新会话沿用现有 Full Access 策略，仅向受信任员工开放。'));}else{toggle(panel,'允许自动创建会话群聊 / 话题',c.auto_create_chat,v=>c.auto_create_chat=v,'会话自动发现功能要求目标连接开启此项。');}list.append(shell);updateConnectionStatuses();
 });
}
function addConnection(){if(busy||!draft||descriptors.length===0)return;let n=1;while(draft.connections.some(c=>c.id==='im-'+n))n++;const c={id:'im-'+n,type:descriptors[0].type,allowed_users:[],admin_users:[],chat_name_prefix:'Codex',auto_create_chat:!['dingtalk','dingtalk_personal'].includes(descriptors[0].type),conversation_mode:'group',shared_chat_id:''};draft.connections.push(c);modified();renderConnections();renderWatch();navigate('connections');}
function renderCodex(){
 const el=$('codex-fields');el.replaceChildren();
 bindValue(el,'收件分析模型（可选）',draft.codex.inbox_model||'',v=>draft.codex.inbox_model=v.trim(),{help:'留空沿用当前 Codex 模型，用于个人收件的关注建议和群消息补全。'});
 const following=(draft.codex.mode||'follow_local')==='follow_local';
 const block=node('div','source-summary');const heading=node('div','connection-header');heading.append(node('h3','','跟随本机 Codex 配置'),node('span','badge',following?'默认模式':'高级兼容模式'));block.append(heading);
 block.append(node('p','','CC Switch 的 Codex 页签负责管理中转与模型，本客户端不再要求填写模型账号或密钥，也不会读取 CC Switch 数据库。'));
 const report=node('div');report.id='codex-report';block.append(report);
 const refresh=node('button','btn','重新检测本机配置');refresh.type='button';refresh.onclick=()=>task(refreshCodex);block.append(refresh);
 const changed=node('div','notice warning');changed.id='codex-change';changed.hidden=true;block.append(changed);el.append(block);
 bindValue(el,'配置来源',draft.codex.mode||'follow_local',v=>{draft.codex.mode=v;if(v==='follow_local'){draft.codex.binary='';draft.codex.home='';draft.codex.model='';}localInfo=null;renderCodex();},{options:[['follow_local','跟随 CC Switch / 本机 Codex（推荐）'],['custom','高级：自定义 CLI 或配置目录']],help:'默认读取当前用户 ~/.codex；不继承旧桥接程序残留的 CODEX_HOME。'});
 if(!following){
  const advanced=node('details','advanced-settings');advanced.open=true;advanced.append(node('summary','','高级设置（通常无需修改）'));
  bindValue(advanced,'Codex CLI（可选）',draft.codex.binary,v=>{draft.codex.binary=v;invalidateLocal();},{pick:()=>api().ChooseExecutable(),help:'留空仍自动检测；找不到 CLI 或需要特定版本时才选择。'});
  bindValue(advanced,'CODEX_HOME（可选）',draft.codex.home,v=>{draft.codex.home=v;invalidateLocal();},{pick:()=>api().ChooseDirectory(),help:'留空使用 ~/.codex；自定义目录必须是 CC Switch 实际写入的目录，否则不会跟随切换。'});
  bindValue(advanced,'旧版模型覆盖（可选）',draft.codex.model,v=>{draft.codex.model=v;invalidateLocal();},{help:'仅为兼容旧配置保留。推荐留空，让 Codex 使用本机模型配置。'});el.append(advanced);
 }
 el.append(node('div','notice','目录由每个会话独立管理：新空白会话自动分配目录；恢复和分叉保留 Codex 原会话目录。创建 IM 连接无需选目录。'));
 const project=node('details','advanced-settings');project.open=!!draft.work_dir;project.append(node('summary','','可选：新会话默认项目'));
 const projectInput=bindValue(project,'默认项目（留空自动分配）',draft.work_dir,v=>{draft.work_dir=v;invalidateLocal();},{pick:()=>api().ChooseDirectory(),help:'仅用于新建会话，不覆盖已有会话。通常无需填写；已有旧配置可点击下方按钮清空。'});
 const clearProject=node('button','btn small','清空，改为每个新会话自动分配');clearProject.type='button';clearProject.onclick=()=>{draft.work_dir='';projectInput.value='';invalidateLocal();modified();};project.append(clearProject);el.append(project);
 bindValue(el,'任务提交方式',draft.codex.submission_mode||'native_queue',v=>draft.codex.submission_mode=v,{options:[['native_queue','当前桌面会话输入（推荐）'],['direct','独立 Codex CLI（App Server）']],help:'此设置控制飞书等渠道的提交方式。桌面模式按会话 ID 提交；独立 CLI 模式不依赖桌面。钉钉始终使用独立 CLI，不受此设置影响。'});
 bindValue(el,'沙箱',draft.codex.sandbox,v=>draft.codex.sandbox=v,{options:[['read-only','只读 · read-only'],['workspace-write','工作区可写 · workspace-write']],help:'这些设置约束辅助 App Server 或直接提交模式；桌面输入模式沿用桌面会话自身权限和审批，不会因这里选择只读而收紧桌面权限。'});
 bindValue(el,'审批策略',draft.codex.approval_policy,v=>draft.codex.approval_policy=v,{options:[['untrusted','非信任操作请求审批 · untrusted'],['on-request','由 Codex 按需请求审批 · on-request']]});
 renderLocalInfo();updateStatus();
}
function invalidateLocal(){localInfo=null;renderLocalInfo();}
function renderLocalInfo(){
 const report=$('codex-report');if(!report)return;report.replaceChildren();
 if(!localInfo){report.append(node('p','muted','配置草稿已变化，请点击“重新检测本机配置”。'));return;}
 report.append(node('p',localInfo.ready?'detected':'detect-error',localInfo.message));
 const rows=[['CLI',localInfo.binary||'未找到'],['配置目录',localInfo.home||'未找到'],['配置文件',localInfo.config_path||'未找到'],['提供商',localInfo.provider||'—'],['模型',localInfo.model||'—']];
 for(const [label,value] of rows){report.append(node('label','',label),node('code','',value));}
 for(const warning of localInfo.warnings||[])report.append(node('p','muted',warning));
}
async function refreshCodex(){if(!draft)return;localInfo=await api().InspectCodex(draft.codex,draft.work_dir);sourceRefreshAt=Date.now();renderLocalInfo();}
function renderLimits(){const el=$('limit-fields');el.replaceChildren();el.append(node('p','muted','授权、提问选择和表单不设等待期限。原请求结束、取消或已处理时，旧操作仍会失效。'));const fields=[['turn_timeout_seconds','会话创建超时（秒）',30,7200],['workers','并发工作者',1,16],['queue_size','每会话等待容量',1,512],['max_message_chars','单条输出字符数',200,3000],['max_input_chars','最大输入字符数',1,64000],['idle_push_ms','进度合并间隔（毫秒）',1000,60000]];
 fields.forEach(([k,label,min,max])=>{const i=bindValue(el,label,draft.limits[k],v=>draft.limits[k]=v,{type:'number',help:`允许范围 ${min} – ${max}`});i.min=min;i.max=max;i.step=1;});
}
function renderWatch(){const el=$('watch-fields');el.replaceChildren();toggle(el,'启用 Codex 会话自动发现',draft.watch.enabled,v=>{draft.watch.enabled=v;renderWatch();},'未设置默认项目时可发现多个目录的 Codex 会话，按目标连接模式创建独立群或共享群话题；请确认这些会话适合分享到目标群成员。');
 const choices=[['','请选择连接'],...draft.connections.map(c=>[c.id,c.id+' · '+(connectionDescriptor(c)?.name||c.type)])];bindValue(el,'同步目标连接',draft.watch.connection_id,v=>draft.watch.connection_id=v,{options:choices,disabled:!draft.watch.enabled});
 const i=bindValue(el,'扫描间隔（秒）',draft.watch.interval_seconds,v=>draft.watch.interval_seconds=v,{type:'number',help:'至少 5 秒；不是即时同步。',disabled:!draft.watch.enabled});i.min=5;i.max=3600;i.step=1;
 toggle(el,'导入启动前的历史会话',draft.watch.import_existing,v=>draft.watch.import_existing=v,'默认关闭，避免意外分享已有会话。');
}
async function load(){const view=await api().GetView();descriptors=view.channels;status=view.status;localInfo=view.codex;sourceRefreshAt=Date.now();applySnapshot(view.snapshot);if(!snapshot.revision)note('欢迎使用。填写 IM 连接并保存即可；工作目录由每个会话独立管理，无需在这里选择。');}
$('save').onclick=()=>task(async()=>{
 const changedTopics=draft.connections.filter(c=>c.conversation_mode==='topic'&&(!snapshot.config.connections.find(old=>old.id===c.id)||snapshot.config.connections.find(old=>old.id===c.id).conversation_mode!=='topic'||snapshot.config.connections.find(old=>old.id===c.id).shared_chat_id!==c.shared_chat_id));
 if(changedTopics.length&&!await confirmAction('启用 / 更换共享群话题模式？','新会话及导入的未绑定会话将集中到共享群，根卡片、历史与回复可能被所有群成员看见。用户白名单不隔离阅读权限。旧群保持原绑定，不迁移、不解散。'))return;
 const enabledAuto=draft.connections.flatMap(c=>(c.personal_auto_reply_users||[]).filter(u=>!(snapshot.config.connections.find(old=>old.id===c.id)?.personal_auto_reply_users||[]).includes(u)).map(u=>c.id+' / '+u));
 if(enabledAuto.length&&!await confirmAction('开启指定联系人的自动回复？','以下联系人新消息在模型明确判断可以处理时，将以你的身份自动回复，无需逐条确认；其余消息仍进入审核。规则保存并重启桥接后生效。\n'+enabledAuto.join('\n')))return;
 const result=await api().Save({config:draft,revision:snapshot.revision,secrets:[...pending.values()]});
 applySnapshot(result);await refreshCodex();note('配置已保存到 SQLite。凭证输入已清空；已有桥接进程继续使用旧配置，需要停止后重新启动。');
});
$('reload').onclick=()=>task(async()=>{if(dirty&&!await confirmAction('丢弃未保存修改？','重新加载会清空当前表单修改和未保存的密钥输入。',{danger:true,confirmText:'丢弃并重新加载'}))return;await load();note('已重新加载本地保存的配置。');});
$('import').onclick=()=>task(async()=>{
 if(dirty&&!await confirmAction('替换当前草稿？','尚未保存的表单与密钥输入将丢弃；数据库暂不更改。'))return;
 const imported=await api().ImportJSON();if(!imported)return;
 if(!await confirmAction('导入配置预览',`文件：${imported.file_name}\n连接数量：${imported.config.connections.length}\n新会话默认项目：${imported.config.work_dir||'自动分配，不覆盖已有会话'}\n\n确认后替换表单草稿，仍需点击保存才会更新数据库。同 ID 连接保留已有本地凭证；被删除连接的凭证在保存时删除。`))return;
 draft=structuredClone(imported.config);pending.clear();modified();render();navigate('connections');invalidateLocal();note('已导入草稿，尚未写入数据库。旧版飞书环境变量字段已忽略；请在客户端填写凭证后保存。');
});
$('start').onclick=()=>task(async()=>{if(dirty)throw new Error('请先保存配置，避免启动旧版本。');await api().StartService();status=await api().GetStatus();updateStatus();note('已提交启动请求，请查看状态。模型或飞书是否可用仍需实际消息确认。');});
$('stop').onclick=()=>task(async()=>{if(!await confirmAction('停止桥接？','这会停止本地接收、同步、等待和未完成审批，不会删除会话数据。已提交到桌面的任务可能仍在排队或执行，请在桌面确认，不要直接重发。',{danger:true,confirmText:'停止'}))return;await api().StopService();status=await api().GetStatus();updateStatus();});
$('add-connection').onclick=addConnection;
document.querySelectorAll('[data-page]').forEach(b=>b.onclick=()=>navigate(b.dataset.page));
document.querySelectorAll('[data-go]').forEach(b=>b.onclick=()=>navigate(b.dataset.go));
// 键盘快捷键：⌘/Ctrl+1…6 切换页面，⌘/Ctrl+S 保存，Esc 关闭提示条。
const pageOrder=[...document.querySelectorAll('.nav[data-page]')].map(b=>b.dataset.page);
document.addEventListener('keydown',e=>{
 if($('confirm-dialog').open)return;
 const mod=e.metaKey||e.ctrlKey;
 if(mod&&!e.shiftKey&&!e.altKey&&/^[1-9]$/.test(e.key)){const target=pageOrder[Number(e.key)-1];if(target){e.preventDefault();if(!busy)navigate(target);}return;}
 if(mod&&e.key.toLowerCase()==='s'){e.preventDefault();if(!$('save').disabled)$('save').click();return;}
 if(e.key==='Escape'&&!$('banner').hidden){dismissNote();}
});
// 禁止页面导航和拖入文件，防止远程页面进入具有本机绑定的 WebView。
document.addEventListener('click',e=>{if(e.target.closest('a'))e.preventDefault();});
window.addEventListener('dragover',e=>e.preventDefault());window.addEventListener('drop',e=>e.preventDefault());
async function poll(){if(polling||busy)return;polling=true;try{status=await api().GetStatus();updateStatus();if(page==='overview'&&Date.now()-eventRefreshAt>5000)await refreshEventDiagnostics();if(page==='codex'&&!dirty&&Date.now()-sourceRefreshAt>10000)await refreshCodex();if(page==='assistant')await window.assistantPage?.poll();}catch{}finally{polling=false;}}
(async()=>{
 for(let attempt=0;attempt<40&&!window.go?.desktop?.API;attempt++)await new Promise(resolve=>setTimeout(resolve,100));
 await task(load);setInterval(poll,2000);
})();

async function refreshEventDiagnostics(){
 const list=$('event-history');if(!list)return;
 try{
  const rows=await api().GetEventDiagnostics();eventRefreshAt=Date.now();list.replaceChildren();
  if(!rows.length){list.append(node('p','muted','尚无消息处理记录。'));return;}
  const phases={not_sent:'尚未发送，可处理原因后重新提交',submission_unconfirmed:'提交结果未确认，请先核对，不要重发',execution_unconfirmed:'已提交，执行结果未确认',execution_failed:'执行失败（已提交，可能有部分结果）',reply_delivery_unconfirmed:'执行已完成，回复同步未确认',delivering_reply:'执行已完成，正在同步回复',received:'收到消息',queued:'本地排队（等待同会话任务／空闲工作者）',waiting_desktop_submit:'等待本机桌面提交入口',checking_desktop:'确认桌面会话与输入区',preserving_draft:'备份并暂存桌面草稿',restoring_draft:'核对并恢复桌面草稿',starting:'开始处理',resuming:'准备 Codex 会话',submitting:'提交 Codex 轮次',waiting_model:'已提交，等待执行／回复',outcome_unconfirmed:'结果未确认，请先检查桌面，不要重发',waiting_desktop:'等待桌面输入框可编辑',waiting_draft:'等待桌面草稿处理',switching_desktop:'正在定位并切换桌面会话',completed:'已发送回复',command_completed:'命令完成',command_failed:'命令失败',binding_failed:'绑定会话失败',queue_full:'队列容量已满'};
  for(const e of rows){const row=node('div','closure-row');row.append(node('p','',new Date(e.received_at*1000).toLocaleString()+' · '+(phases[e.stage]||e.stage||'旧版未记录阶段')+' · '+e.status));if(e.summary)row.append(node('p','detect-error',e.summary));if(e.updated_at)row.append(node('small','muted','最后更新：'+new Date(e.updated_at*1000).toLocaleString()));list.append(row);}
 }catch{list.replaceChildren(node('p','muted','暂时无法读取处理记录。'));}
}


async function refreshBackground(){
 const el=$('background-settings');if(!el)return;
 const info=await api().GetBackgroundInfo();el.replaceChildren(node('p','muted',info.message));
 const button=node('button','btn',info.auto_start?'停用登录自启':'启用登录自启');
 button.onclick=()=>task(async()=>{if(!await confirmAction('修改后台自启？','请先停止桥接。此操作会安装或卸载当前用户的 macOS 登录启动项；不会更改模型权限。'))return;await api().SetAutoStart(!info.auto_start);await refreshBackground();});el.append(button);
 const update=node('button','btn','更新后台程序（需先停止桥接）');
 update.onclick=()=>task(async()=>{if(!await confirmAction('更新后台程序？','只在桥接停止后替换后台程序。已有任务不会重放，登录自启设置将保留。完成后请重新启动桥接。'))return;await api().UpdateBackground();note('后台程序已更新，请启动桥接。');await refreshBackground();});el.append(update);
}
// 桌面连接管理只由用户点击触发，不纳入自动刷新或任务重放。
let desktopLaunchState=null;
function renderDesktopLaunch(s){
 desktopLaunchState=s;
 const list=$('desktop-launch-app');list.replaceChildren();
 const apps=s.applications||[];
 if(!apps.length){const o=node('option','','未发现可用应用');o.value='';list.append(o);}
 else {
  if(!s.selected){const o=node('option','','请选择应用后重新检测');o.value='';list.append(o);}
  for(const a of apps){const o=node('option','',`${a.name} · ${a.version||'未知版本'} · ${a.path}`);o.value=a.path;list.append(o);}
 }
 list.value=s.selected||'';list.disabled=apps.length<2;
 const box=$('desktop-launch-status');box.replaceChildren();
 const labels={ready:'入口可用',stopped:'桌面未运行',running_without_endpoint:'桌面运行中，入口不可用',select_application:'需选择应用',missing:'未找到应用',untrusted:'身份校验未通过',unsafe_listener:'连接地址不安全',ambiguous:'桌面进程不唯一',unknown:'状态未确认',unsupported:'系统暂不支持',launch_unconfirmed:'启动结果待确认'};
 box.append(node('strong','',labels[s.phase]||'状态待核对'),node('p','',s.message||''));
 if(s.selected)box.append(node('p','muted','应用：'+s.selected));
 if(s.port)box.append(node('p','muted','本机连接端口：'+s.port));
 box.append(node('p','muted','IM连接、桌面入口、模型执行是三个不同状态；入口可用不代表所有任务一定成功。'));
 $('desktop-launch-start').disabled=s.phase!=='stopped';
}
$('desktop-launch-check').onclick=()=>task(async()=>{
 if(!api().InspectDesktopApplication)throw new Error('请重新构建并打开新版客户端以使用桌面启动入口。');
 renderDesktopLaunch(await api().InspectDesktopApplication($('desktop-launch-app').value||''));
});
$('desktop-launch-app').onchange=()=>{
 desktopLaunchState=null;$('desktop-launch-start').disabled=true;
 $('desktop-launch-status').replaceChildren(node('p','muted','应用选择已改变，请重新检测后再启动。'));
};
$('desktop-launch-start').onclick=()=>task(async()=>{
 if(!desktopLaunchState||desktopLaunchState.phase!=='stopped')throw new Error('请先检测并确认桌面未运行。');
 if(!await confirmAction('尝试启动桌面连接模式？','将打开所选桌面应用并尝试启用仅本机连接入口。调试入口能访问桌面内容，请仅在可信电脑使用。不会退出已运行的应用，不提交问题；若当前版本不支持，只报告未确认，不自动重试。'))return;
 renderDesktopLaunch(await api().LaunchDesktopApplication(desktopLaunchState.selected,true));
 note(desktopLaunchState.ready?'桌面入口已确认可用；请自行发送新的验收问题，旧问题不会重放。':desktopLaunchState.message,!desktopLaunchState.ready);
});
