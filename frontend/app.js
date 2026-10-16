/* 原生 Wails 绑定；无远程脚本、无前端依赖、无凭证 localStorage。 */
'use strict';
const $ = id => document.getElementById(id);
const titles = {overview:'概览',connections:'IM 连接',codex:'Codex 配置',limits:'运行限制',watch:'会话同步',tasks:'任务与附件'};
let snapshot, draft, descriptors=[], status={}, localInfo=null, pending=new Map(), dirty=false, busy=false;
let sourceRefreshAt=0, closureRefreshAt=0, presentationRefreshAt=0, eventRefreshAt=0;
let savedIDs=new Set(), page='overview', polling=false;
const historyPageState=new Map(),connectionExpansion=new Map();
function historyPage(kind){if(!historyPageState.has(kind))historyPageState.set(kind,{page:1,size:20,generation:0});return historyPageState.get(kind);}
async function loadHistoryPage(kind){
 const state=historyPage(kind),generation=++state.generation,requested=state.page,size=state.size;
 let result;
 try{result=await api().GetHistoryPage(kind,requested,size);}catch(error){if(generation!==state.generation||state.page!==requested||state.size!==size)return null;throw error;}
 if(generation!==state.generation||state.page!==requested||state.size!==size)return null;
 state.page=result.page;state.size=result.page_size;return result;
}
function appendHistoryPager(parent,result,refresh){
 const state=historyPage(result.kind),pages=Math.max(1,Math.ceil(result.total/result.page_size));
 const bar=node('nav','history-pager');bar.setAttribute('aria-label','列表分页');
 bar.append(node('span','muted',`共 ${result.total} 条 · 第 ${result.page}/${pages} 页`));
 const go=target=>{if(busy)return;state.page=Math.max(1,Math.min(pages,target));state.generation++;task(async()=>{await refresh();const bar=parent.querySelector('.history-pager');if(bar&&page!=='overview')bar.scrollIntoView({block:'nearest'});});};
 for(const [label,target,disabled] of [['首页',1,result.page<=1],['上一页',result.page-1,result.page<=1],['下一页',result.page+1,result.page>=pages],['末页',pages,result.page>=pages]]){
  const button=node('button','btn small',label);button.type='button';button.disabled=disabled;button.onclick=()=>go(target);bar.append(button);
 }
 const size=node('select');size.setAttribute('aria-label','每页条数');
 for(const value of [10,20,50]){const option=node('option','',`${value} 条/页`);option.value=String(value);size.append(option);}size.value=String(result.page_size);
 size.onchange=()=>{if(busy){size.value=String(state.size);return;}state.size=Number(size.value);state.page=1;state.generation++;task(refresh);};bar.append(size);
 parent.append(bar);if(result.warning)parent.append(node('p','notice warning',result.warning));
}
function localHistoryPage(kind,rows){const state=historyPage(kind);state.page=Math.max(1,Math.min(state.page,Math.max(1,Math.ceil(rows.length/state.size))));return {kind,page:state.page,page_size:state.size,total:rows.length,rows:rows.slice((state.page-1)*state.size,state.page*state.size)};}
function updateConnectionStatuses(){
 const labels={connected:'已连接',connecting:'连接中',reconnecting:'重连中',disconnected:'已断开',failed:'异常',stopped:'已停止'};
 document.querySelectorAll('.connection-live-status').forEach(el=>{const current=(status.connections||[]).find(c=>c.connection===el.dataset.connection);el.textContent=current?(labels[current.phase]||'状态待确认'):'尚未连接';});
}

function api(){const value=window.go?.desktop?.API;if(!value)throw new Error('此页面需要在 IM Agent 桌面客户端中打开，不能直接用浏览器打开 HTML。');return value;}
function node(tag,cls,text){const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;}
function note(text,error=false){$('banner').hidden=false;$('banner').textContent=String(text);$('banner').className=error?'error':'';}
function modified(){dirty=true;api().SetDirty(true).catch(()=>{});updateHeader();}
function updateHeader(){
 $('dirty-badge').textContent=dirty?'有未保存修改':'已保存';$('dirty-badge').className='badge'+(dirty?' unsaved':'');
 $('save').disabled=busy||!draft||!dirty;$('reload').disabled=busy;$('import').disabled=busy;
 $('add-connection').disabled=busy||!draft;
 updateStatus();
}
async function task(fn){if(busy)return;busy=true;document.body.classList.add('working');$('busy').hidden=false;updateHeader();
 try{await fn();}catch(e){note(String(e?.message||e),true);}finally{busy=false;document.body.classList.remove('working');$('busy').hidden=true;updateHeader();}
}
function confirmAction(title,message){return new Promise(resolve=>{
 $('confirm-title').textContent=title;$('confirm-message').textContent=message;
 const d=$('confirm-dialog');let finished=false;
 const done=value=>{if(finished)return;finished=true;d.close();d.removeEventListener('cancel',cancel);resolve(value);};
 const cancel=e=>{e.preventDefault();done(false);};d.addEventListener('cancel',cancel);
 $('confirm-no').onclick=()=>done(false);$('confirm-yes').onclick=()=>done(true);d.showModal();
 });}
function navigate(p){if(p!=='tasks'){historyPage('drafts').generation++;clearTaskDelivery();if($('desktop-draft-list')){$('desktop-draft-list').replaceChildren();desktopDraftSignature='';}}page=p;Object.keys(titles).forEach(key=>$('page-'+key).hidden=key!==p);document.querySelectorAll('.nav').forEach(n=>n.classList.toggle('active',n.dataset.page===p));$('breadcrumb').textContent=titles[p];if(p==='tasks')task(refreshTaskCenter);if(p==='overview')refreshBackground().catch(()=>{});}
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
 const reportSend=$('task-delivery-send');if(reportSend)reportSend.disabled=busy||status.phase!=='running';
 const names={stopped:'已停止',starting:'启动中',running:'运行中',stopping:'停止中',failed:'启动或运行失败'};
 const phase=status.phase||'stopped';$('status-title').textContent=names[phase]||phase;
 $('status-dot').className='dot '+phase;
 $('status-heading').textContent=phase==='running'?'你的桥接进程正在运行':phase==='starting'?'正在准备桥接连接':phase==='stopping'?'正在安全停止':phase==='failed'?'需要检查连接配置':'由你决定何时开始';
 $('status-message').textContent=status.message||'客户端不会自动启动桥接。';
 $('policy-notice').hidden=!status.policy_notice;$('policy-notice').textContent=status.policy_notice||'';
 if(phase==='running'){
  const connections=status.connections||[],scan=status.diagnostics||{};
  if(connections.some(c=>c.phase==='failed'||c.phase==='disconnected')){
   $('status-title').textContent='进程运行 · IM 连接异常';$('status-heading').textContent='IM 连接需要检查';$('status-dot').className='dot failed';
  }else if(scan.intake_phase==='degraded'){
   $('status-title').textContent='IM 已连接 · 收件对账异常';$('status-heading').textContent='收件对账需要检查';$('status-dot').className='dot failed';
  }else if(scan.scan_phase==='failed'){
   $('status-title').textContent='进程运行 · 自动同步失败';$('status-heading').textContent='会话同步遇到问题';$('status-dot').className='dot failed';
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
 $('scan-health').textContent='自动同步：'+(diag.scan_message||'尚未开始');
 $('message-health').textContent='消息处理：'+(diag.message||'尚未收到通过白名单的消息');
 const intake=$('intake-health');if(intake)intake.textContent='收件对账：'+(diag.intake||'尚未完成群历史对账；连接正常不代表消息已收到');
 const live=$('live-sync-health');if(live)live.textContent='实时消息：'+(diag.live_sync||'启动后从新消息开始逐条同步');

}
function renderConnections(){
 const list=$('connection-list');list.replaceChildren();
 if(!draft.connections.length){const empty=node('div','panel empty');empty.append(node('div','symbol','▧'),node('h2','','添加第一个 IM 连接'),node('p','','凭证仅保存在本机，只有白名单用户可以使用机器人。'));const b=node('button','btn primary','＋ 添加连接');b.onclick=addConnection;empty.append(b);list.append(empty);return;}
 draft.connections.forEach(c=>{
  const d=connectionDescriptor(c);const shell=node('details','panel connection connection-fold'),panel=node('div','connection-fields'),head=node('summary','connection-header'),intro=node('div');shell.open=connectionExpansion.has(c.id)?connectionExpansion.get(c.id):!savedIDs.has(c.id);shell.ontoggle=()=>connectionExpansion.set(c.id,shell.open);intro.append(node('h3','',d?.name||c.type),node('p','',c.id||'新连接'));const remove=node('button','btn danger small','删除连接');remove.onclick=()=>task(async()=>{if(!await confirmAction('删除连接？','保存后会删除该连接的本地加密凭证；业务历史保留。正在运行的连接不会立即停止。'))return;draft.connections=draft.connections.filter(x=>x!==c);if(draft.watch.connection_id===c.id){draft.watch.enabled=false;draft.watch.connection_id='';}for(const [k,v] of pending){if(v.connection_id===c.id)pending.delete(k);}modified();renderConnections();renderWatch();});const health=node('span','badge connection-live-status');health.dataset.connection=c.id;head.append(intro,health,node('span','muted','展开 / 收起'));shell.append(head,panel);panel.append(remove);
  const basic=node('div','two-cols');
  bindValue(basic,'连接 ID',c.id,value=>{const old=c.id;connectionExpansion.set(value,shell.open);if(old!==value)connectionExpansion.delete(old);c.id=value;health.dataset.connection=value;for(const [k,v] of [...pending]){if(v.connection_id===old){pending.delete(k);v.connection_id=value;pending.set(value+'\0'+v.field,v);}}head.querySelector('p').textContent=value;renderWatch();},{disabled:savedIDs.has(c.id),help:'唯一标识；保存后不可直接改名，避免凭证与会话关联错位。'});
  bindValue(basic,'渠道',c.type,value=>{c.type=value;if(!descriptors.find(x=>x.type===value)?.supports_topics)c.conversation_mode='group';for(const [k,v] of pending){if(v.connection_id===c.id)pending.delete(k);}renderConnections();},{disabled:savedIDs.has(c.id),options:descriptors.map(x=>[x.type,x.name])});panel.append(basic);
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
  toggle(panel,'允许自动创建会话群聊 / 话题',c.auto_create_chat,v=>c.auto_create_chat=v,'会话自动发现功能要求目标连接开启此项。');list.append(shell);updateConnectionStatuses();
 });
}
function addConnection(){if(busy||!draft||descriptors.length===0)return;let n=1;while(draft.connections.some(c=>c.id==='im-'+n))n++;const c={id:'im-'+n,type:descriptors[0].type,allowed_users:[],admin_users:[],chat_name_prefix:'Codex',auto_create_chat:true,conversation_mode:'group',shared_chat_id:''};draft.connections.push(c);modified();renderConnections();renderWatch();navigate('connections');}
function renderCodex(){
 const el=$('codex-fields');el.replaceChildren();
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
 bindValue(el,'任务提交方式',draft.codex.submission_mode||'native_queue',v=>draft.codex.submission_mode=v,{options:[['native_queue','当前桌面会话输入（推荐）'],['direct','兼容：App Server 直接提交']],help:'推荐模式连接本机 Codex++ / CDP 桌面端，先核对当前会话 ID，再提交到桌面输入框。关闭自动切换时需手动打开目标会话；开启后定位并切换已加载的目标。发送前核对 ID，不自动回退 CLI。'});
 toggle(el,'自动切换到消息所属的 Codex 会话',draft.codex.auto_switch_thread,v=>draft.codex.auto_switch_thread=v,'开启后，飞书消息可能切换电脑上显示的会话。按已加载侧栏的会话 ID 精确定位；有草稿最多等待 120 秒并提示，不覆盖内容。切换成功后仍严格核对 ID；不支持时明确拒绝。保存后重启桥接生效。');
 toggle(el,'飞书优先提交并保留纯文本草稿（兼容模式）',!!draft.codex.preserve_desktop_draft,v=>draft.codex.preserve_desktop_draft=v,'默认关闭，仅桌面输入模式有效。只暂存当前目标会话可确认的无附件纯文本（最多64KiB）：先在本机加密备份，自动释放输入焦点并复核，文本变化则追加快照，再移走并优先提交；不要求手工确认输入法状态。候选列表及撤销栈不在备份范围。自动恢复另有独立开关；关闭时只保留本机快照，不会自动粘贴。保存后重启生效，未完成实机验收前请勿用于重要草稿。');
 toggle(el,'自动恢复桌面草稿（实验，默认关闭）',!!draft.codex.auto_restore_desktop_draft,v=>draft.codex.auto_restore_desktop_draft=v,'只有同时开启上面的草稿保留后才有效。默认关闭：飞书提交完成后只保留加密快照，需在“任务与附件 → 草稿恢复（仅本机）”手动查看或导出。开启后仍可能因真实编辑器焦点、重载或输入事件变化而恢复失败，重要草稿不要使用。保存后重启生效。');
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
 const result=await api().Save({config:draft,revision:snapshot.revision,secrets:[...pending.values()]});
 applySnapshot(result);await refreshCodex();note('配置已保存到 SQLite。凭证输入已清空；已有桥接进程继续使用旧配置，需要停止后重新启动。');
});
$('reload').onclick=()=>task(async()=>{if(dirty&&!await confirmAction('丢弃未保存修改？','重新加载会清空当前表单修改和未保存的密钥输入。'))return;await load();note('已重新加载本地保存的配置。');});
$('import').onclick=()=>task(async()=>{
 if(dirty&&!await confirmAction('替换当前草稿？','尚未保存的表单与密钥输入将丢弃；数据库暂不更改。'))return;
 const imported=await api().ImportJSON();if(!imported)return;
 if(!await confirmAction('导入配置预览',`文件：${imported.file_name}\n连接数量：${imported.config.connections.length}\n新会话默认项目：${imported.config.work_dir||'自动分配，不覆盖已有会话'}\n\n确认后替换表单草稿，仍需点击保存才会更新数据库。同 ID 连接保留已有本地凭证；被删除连接的凭证在保存时删除。`))return;
 draft=structuredClone(imported.config);pending.clear();modified();render();navigate('connections');invalidateLocal();note('已导入草稿，尚未写入数据库。旧版飞书环境变量字段已忽略；请在客户端填写凭证后保存。');
});
$('start').onclick=()=>task(async()=>{if(dirty)throw new Error('请先保存配置，避免启动旧版本。');await api().StartService();status=await api().GetStatus();updateStatus();note('已提交启动请求，请查看状态。模型或飞书是否可用仍需实际消息确认。');});
$('stop').onclick=()=>task(async()=>{if(!await confirmAction('停止桥接？','这会停止本地接收、同步、等待和未完成审批，不会删除会话数据。已提交到桌面的任务可能仍在排队或执行，请在桌面确认，不要直接重发。'))return;await api().StopService();status=await api().GetStatus();updateStatus();});
$('add-connection').onclick=addConnection;
document.querySelectorAll('[data-page]').forEach(b=>b.onclick=()=>navigate(b.dataset.page));
document.querySelectorAll('[data-go]').forEach(b=>b.onclick=()=>navigate(b.dataset.go));
// 禁止页面导航和拖入文件，防止远程页面进入具有本机绑定的 WebView。
document.addEventListener('click',e=>{if(e.target.closest('a'))e.preventDefault();});
window.addEventListener('dragover',e=>e.preventDefault());window.addEventListener('drop',e=>e.preventDefault());
async function poll(){if(polling||busy)return;polling=true;try{status=await api().GetStatus();updateStatus();if(page==='overview'&&Date.now()-eventRefreshAt>5000)await refreshEventDiagnostics();if(page==='codex'&&!dirty&&Date.now()-sourceRefreshAt>10000)await refreshCodex();if(page==='tasks'&&Date.now()-taskCenterAt>5000)await refreshTaskCenter();if(page==='watch'&&Date.now()-closureRefreshAt>5000)await refreshClosures();if(page==='watch'&&Date.now()-presentationRefreshAt>5000)await refreshPresentations();}catch{}finally{polling=false;}}
(async()=>{
 for(let attempt=0;attempt<40&&!window.go?.desktop?.API;attempt++)await new Promise(resolve=>setTimeout(resolve,100));
 await task(load);setInterval(poll,2000);
})();

async function refreshClosures(){
 const list=$('closure-list'),warning=$('closure-warning');
 try{
  const result=await loadHistoryPage('closures');if(!result)return;closureRefreshAt=Date.now();warning.hidden=!result.warning;warning.textContent=result.warning||'';list.replaceChildren();appendHistoryPager(list,{...result,warning:''},refreshClosures);
  if(!result.closures.length){list.append(node('p','muted','暂无双向关闭记录。启动桥接后监测已绑定会话；需要在飞书后台订阅群解散事件。'));return;}
  const labels={pending:'待同步 / 重试中',done:'两端已关闭',protected:'原群受保护',topic_archived:'话题会话已归档 · 共享群保留'};
  for(const item of result.closures){
   const row=node('div','closure-row');const top=node('div','connection-header');top.append(node('strong','',item.thread_id.slice(0,12)),node('span','badge',item.state==='pending'&&item.chat_closed&&!item.codex_archived?'群已解散 · 会话待归档':(labels[item.state]||item.state)));row.append(top);
   row.append(node('p','',`${item.connection} · ${item.chat_id}${item.topic_id?' · 话题 '+item.topic_id:''}`));
   row.append(node('p','',item.topic_id?`Codex：${item.codex_archived?'已归档':'待确认'}　话题入口：${item.state==='topic_archived'?'已标记归档':'待更新'}　共享群不解散`:`Codex：${item.codex_archived?'已归档':'待确认'}　群聊：${item.chat_closed?'已解散':'未确认解散'}　重试次数：${item.attempts}`));
   if(item.last_error)row.append(node('p','detect-error',item.last_error));
   if(item.state==='pending'){
    if(item.next_attempt)row.append(node('p','muted','下次尝试：'+new Date(item.next_attempt*1000).toLocaleString()));
    const retry=node('button','btn small','立即重试');retry.disabled=status.phase!=='running';retry.onclick=()=>task(async()=>{
     if(!await confirmAction('重试关闭同步？',item.topic_id?'将继续归档对应 Codex 会话并更新话题入口，不解散共享群。':(item.chat_closed?'将先核对归档结果；上次归档回执未知时仅查询，不重复提交。桌面任务仍在执行或兼容入口不可用时继续等待，不强制关闭桌面。':'将继续归档 Codex，或解散有客户端创建证明的对应独立群聊。此操作不会恢复会话。')))return;
     await api().RetryClosure(item.thread_id);await refreshClosures();note('已提交重试，处理结果请查看同步记录。');
    });row.append(retry);
   }
   list.append(row);
  }
 }catch{warning.hidden=false;warning.textContent='无法读取双向同步记录，请稍后刷新。';}
}
$('refresh-closures').onclick=()=>task(refreshClosures);

async function refreshPresentations(){
 const list=$('presentation-list'),warning=$('presentation-error');
 try{
  const result=await loadHistoryPage('presentations');if(!result)return;const rows=result.presentations;presentationRefreshAt=Date.now();list.replaceChildren();warning.hidden=!result.warning;warning.textContent=result.warning||'';appendHistoryPager(list,{...result,warning:''},refreshPresentations);
  if(!rows.length){list.append(node('p','muted','暂无补齐记录。启动桥接后处理有创建证明的绑定群。'));return;}
  const historyLabels={pending:'待同步',sending:'发送中',sent:'已同步最近一轮',empty:'当前没有可展示的对话',unknown:'送达不确定，停止自动重发'};
  const groups=new Map();
  for(const p of rows){
   let parent=list;
   if(p.topic_id){
    const key=JSON.stringify([p.connection,p.chat_id]);
    if(!groups.has(key)){
     const block=node('section','topic-group');block.append(node('h4','','共享群 · '+p.connection),node('p','muted',p.chat_id));
     const children=node('div','topic-children');block.append(children);list.append(block);groups.set(key,children);
    }
    parent=groups.get(key);
   }
   const row=node('div','closure-row'),top=node('div','connection-header');top.append(node('strong','',p.name||p.thread_id.slice(-8)),node('span','badge',p.topic_id?(p.name_applied?'话题入口已设置':'话题入口待设置'):(p.name_applied?'群名已设置':'群名待设置')));row.append(top);
   row.append(node('p','',p.topic_id?'话题：'+p.topic_id+' · Codex：'+p.thread_id.slice(-8):p.connection+' · '+p.chat_id));row.append(node('p','',`历史：${historyLabels[p.history_state]||p.history_state}（${p.history_count} 条）`));
   if(p.session_status!=='active')row.append(node('p','muted','会话已关闭或关闭中，不再补齐。'));if(p.name_error)row.append(node('p','detect-error',p.name_error));if(p.history_error)row.append(node('p','detect-error',p.history_error));
   if(p.session_status==='active'&&(!p.name_applied||p.history_state==='pending')&&!['sending','unknown'].includes(p.history_state)){
    const retry=node('button','btn small','提前重试');retry.disabled=status.phase!=='running';retry.onclick=()=>task(async()=>{
     await api().RetryPresentation(p.thread_id);await refreshPresentations();note('已安排重试；已送达的历史不会重发。');
    });row.append(retry);
   }
   parent.append(row);
  }
 }catch{warning.hidden=false;warning.textContent='读取补齐状态失败，请稍后刷新。';}
}
$('refresh-presentations').onclick=()=>task(refreshPresentations);

async function refreshEventDiagnostics(){
 const list=$('event-history');if(!list)return;
 try{
  const rows=await api().GetEventDiagnostics();eventRefreshAt=Date.now();list.replaceChildren();
  if(!rows.length){list.append(node('p','muted','尚无消息处理记录。'));return;}
  const phases={not_sent:'尚未发送，可处理原因后重新提交',submission_unconfirmed:'提交结果未确认，请先核对，不要重发',execution_unconfirmed:'已提交，执行结果未确认',execution_failed:'执行失败（已提交，可能有部分结果）',reply_delivery_unconfirmed:'执行已完成，回复同步未确认',delivering_reply:'执行已完成，正在同步回复',received:'收到消息',queued:'本地排队（等待同会话任务／空闲工作者）',waiting_desktop_submit:'等待本机桌面提交入口',checking_desktop:'确认桌面会话与输入区',preserving_draft:'备份并暂存桌面草稿',restoring_draft:'核对并恢复桌面草稿',starting:'开始处理',resuming:'准备 Codex 会话',submitting:'提交 Codex 轮次',waiting_model:'已提交，等待执行／回复',outcome_unconfirmed:'结果未确认，请先检查桌面，不要重发',waiting_desktop:'等待桌面输入框可编辑',waiting_draft:'等待桌面草稿处理',switching_desktop:'正在定位并切换桌面会话',completed:'已发送回复',command_completed:'命令完成',command_failed:'命令失败',binding_failed:'绑定会话失败',queue_full:'队列容量已满'};
  for(const e of rows){const row=node('div','closure-row');row.append(node('p','',new Date(e.received_at*1000).toLocaleString()+' · '+(phases[e.stage]||e.stage||'旧版未记录阶段')+' · '+e.status));if(e.summary)row.append(node('p','detect-error',e.summary));if(e.updated_at)row.append(node('small','muted','最后更新：'+new Date(e.updated_at*1000).toLocaleString()));list.append(row);}
 }catch{list.replaceChildren(node('p','muted','暂时无法读取处理记录。'));}
}


let fileThread='',fileKind='file',fileChoices=[],taskCenterAt=0;
const taskStageNames={not_sent:'尚未发送，可处理原因后重新提交',submission_unconfirmed:'提交结果未确认，请先核对，不要重发',execution_unconfirmed:'已提交，执行结果未确认',execution_failed:'执行失败（已提交，可能有部分结果）',reply_delivery_unconfirmed:'执行已完成，回复同步未确认',delivering_reply:'执行已完成，正在同步回复',waiting_caption:'附件等待任务说明',images_bundled:'附件已合并到任务',images_cancelled:'已取消暂存附件',images_expired:'附件已过期（未执行）',image_batch_failed:'附件组合失败',queued:'本地排队（等待同会话任务／空闲工作者）',waiting_desktop_submit:'等待本机桌面提交入口',checking_desktop:'确认桌面会话与输入区',preserving_draft:'备份并暂存桌面草稿',restoring_draft:'核对并恢复桌面草稿',received:'已接收',starting:'开始处理',resuming:'准备会话',submitting:'提交输入',waiting_model:'已提交，等待执行／回复',completed:'已发送回复',downloading_attachment:'下载附件',waiting_draft:'等待桌面草稿处理',waiting_desktop:'等待输入框',switching_desktop:'切换会话',outcome_unconfirmed:'执行结果待核对',command_completed:'命令完成',command_failed:'命令失败',binding_failed:'会话绑定失败'};
const transferNames={sent:'已送达',received:'已保存',sending:'发送中',downloading:'下载中',failed:'失败',unknown:'结果待核对'};
const taskStatusNames={waiting_input:'等待文字说明',bundled:'已合并提交',cancelled:'已取消',expired:'已过期（未执行）',processing:'处理中',done:'完成',failed:'失败',uncertain:'结果未确认',interrupted:'上次运行中断（未自动重放）',queued_failed:'排队失败'};
async function refreshBackground(){
 const el=$('background-settings');if(!el)return;
 const info=await api().GetBackgroundInfo();el.replaceChildren(node('p','muted',info.message));
 const button=node('button','btn',info.auto_start?'停用登录自启':'启用登录自启');
 button.onclick=()=>task(async()=>{if(!await confirmAction('修改后台自启？','请先停止桥接。此操作会安装或卸载当前用户的 macOS 登录启动项；不会更改模型权限。'))return;await api().SetAutoStart(!info.auto_start);await refreshBackground();});el.append(button);
 const update=node('button','btn','更新后台程序（需先停止桥接）');
 update.onclick=()=>task(async()=>{if(!await confirmAction('更新后台程序？','只在桥接停止后替换后台程序。已有任务不会重放，登录自启设置将保留。完成后请重新启动桥接。'))return;await api().UpdateBackground();note('后台程序已更新，请启动桥接。');await refreshBackground();});el.append(update);
}
function renderRequestResult(row,result){
 if(!result)return;
 const execution={submitted:'已提交，执行结果待确认',completed:'执行已确认完成',failed:'执行已确认失败/中断'};
 const delivery={waiting_execution:'等待执行结果',awaiting_replies:'执行已完成，回复清单待核对',pending:'等待全部回复确认',sent:'全部回复已确认送达',not_applicable:'没有已确认完成的回复清单'};
 row.append(node('p','',`${execution[result.execution]||'执行状态待核对'} · ${delivery[result.delivery]||'投递状态待核对'}`));
 if(result.expected_parts>0)row.append(node('p','muted',`回复片段确认：${result.confirmed_parts}/${result.expected_parts}`));
 if(result.recovered)row.append(node('p','muted','已依据完整回复清单恢复为完成；没有重新执行问题。'));
 if(result.completed_at)row.append(node('p','muted','执行确认：'+new Date(result.completed_at*1000).toLocaleString()));
 if(result.delivered_at)row.append(node('p','muted','送达确认：'+new Date(result.delivered_at*1000).toLocaleString()));
 if(result.observation_error)row.append(node('p','detect-error','只读恢复观察：'+result.observation_error));
 if((result.audit||[]).length){
  const details=node('details','');details.append(node('summary','','最近状态确认记录'));
  const names={execution_completed:'确认执行完成',request_unfinished:'请求未完成',reply_delivered:'回复确认送达',reply_delivery_recovered:'回复恢复送达',execution_aborted:'确认执行中断'};
  for(const a of result.audit){details.append(node('p','muted',`${new Date(a.at*1000).toLocaleString()} · ${names[a.reason]||a.reason} · 原状态：${taskStatusNames[a.previous_status]||a.previous_status} / ${taskStageNames[a.previous_stage]||a.previous_stage}`));if(a.detail)details.append(node('p','',a.detail));}
  row.append(details);
 }
}
// 展示全量本地阶段计数，不从任务列表截断数据推算队列名次。
function renderWaitingSummary(parent,waiting){
 if(!waiting)return;
 const row=node('div','closure-row');
 row.append(node('strong','','本地等待记录（跨会话）'));
 const counts=[['本地排队',waiting.queued],['等待桌面提交入口',waiting.desktop_submit],['等待草稿处理',waiting.draft],['桌面确认／草稿处理',waiting.desktop_ready],['已提交，等待执行／回复',waiting.model]];
 for(const [label,count] of counts)row.append(node('p','',`${label}：${count} 项`));
 row.append(node('small','muted','快照时间：'+new Date(waiting.captured_at*1000).toLocaleString()));
 row.append(node('p','muted','仅统计本地标记为处理中的对应阶段记录，不受下方最多150条任务列表限制；不是模型服务端队列，不表示排队名次或预计完成时间。阶段变化、断线或后台停止时记录可能滞后。'));
 if(status.phase!=='running')row.append(node('p','muted','后台当前未显示为运行中：以上是留存记录，不代表仍在排队或会自动重放。'));
 parent.append(row);
}
let desktopDraftSignature='';
async function refreshDesktopDrafts(){
 const list=$('desktop-draft-list');if(!list)return;
 const state=historyPage('drafts'),generation=++state.generation,requested=state.page,size=state.size;
 const stale=()=>generation!==state.generation||requested!==state.page||size!==state.size||page!=='tasks';
 try{
  const view=await api().GetDesktopDrafts(),records=view.records||[];
  if(stale())return;
  const paged=localHistoryPage('drafts',records.slice().reverse());const signature=JSON.stringify([view,paged.page,paged.page_size]);if(signature===desktopDraftSignature)return;
  desktopDraftSignature=signature;list.replaceChildren();appendHistoryPager(list,paged,refreshDesktopDrafts);
  if(view.warning)list.append(node('p','detect-error',view.warning));
  if(!records.length){list.append(node('p','muted',view.warning?'当前没有可列出的完整记录；不代表快照文件已丢失。':'没有已保存的桌面草稿快照。'));return;}
  const states={saved:'已备份，是否移走需核对',suspended:'已暂存，恢复待确认',retained:'请核对并手动恢复',restored:'已确认自动恢复（备份仍保留）'};
  const reasons={auto_restore_disabled:'自动恢复已关闭，仅保留本机快照',restore_prepare_rpc_failed:'恢复准备通信失败',restore_prepare_rejected:'恢复准备被保护条件拒绝',restore_wait_timeout:'等待输入框清空超时',restore_apply_rpc_failed:'恢复插入通信失败',restore_apply_rejected:'恢复插入被保护条件拒绝',restore_blocked:'恢复期间检测到用户或桌面状态变化',restore_target_changed:'恢复目标会话或编辑器已变化',restore_structure_changed:'恢复时编辑器结构发生变化',restore_not_empty:'输入框尚未确认为空',restore_focus_changed:'恢复时输入框焦点已变化',restore_command_failed:'桌面未接受恢复输入',restore_input_unobserved:'未观察到恢复输入事件',restore_content_mismatch:'恢复后的文本与快照不一致',restore_confirm_rpc_failed:'恢复确认通信失败',restore_confirm_rejected:'恢复确认被保护条件拒绝',snapshot_state_unconfirmed:'恢复成功但快照状态未确认'};
  for(const record of paged.rows){
   const row=node('div','closure-row');
   row.append(node('strong','',states[record.state]||'状态待核对'),node('p','muted',`会话：${record.thread_id} · ${new Date(record.created_at).toLocaleString()}`),node('p','detect-error',record.state==='retained'&&record.reason?`恢复原因：${reasons[record.reason]||record.reason}`:''),node('code','',record.id));
   const show=node('button','btn small','查看草稿原文（仅本机）');
   show.onclick=()=>task(async()=>{
    if(!await confirmAction('在本机显示这条草稿？','原文可能包含隐私信息，只在当前客户端显示，不上传飞书、不写剪贴板、不自动粘贴或发送。请核对目标会话后自行复制恢复。'))return;
    const text=await api().ReadDesktopDraft(record.id,true);
    const field=node('textarea');field.readOnly=true;field.rows=8;field.value=text;field.style.width='100%';field.setAttribute('aria-label','草稿原文（只读，仅本机）');
    const hide=node('button','btn small','隐藏原文');hide.onclick=()=>{field.value='';field.remove();hide.remove();show.disabled=false;};
    show.disabled=true;row.append(field,hide);
   });
   const exportFile=node('button','btn small','导出原文（本机明文文件）');
   exportFile.onclick=()=>task(async()=>{
    if(!await confirmAction('导出草稿明文？','导出文件包含完整草稿原文，请选择可信的本机非云同步目录和新文件名。程序不主动上传，但同步软件可能上传所选目录中的文件。不会覆盖已有文件、写剪贴板或自动发送。'))return;
    const path=await api().ExportDesktopDraft(record.id,true);if(path)note('草稿原文已导出到所选本机文件；请保管好明文文件，加密快照仍保留。');
   });
   row.append(show,exportFile,node('p','muted','屏幕仅作预览；如需精确保留原始换行，请显式导出原文，不以预览框复制结果作为字节级备份。'));
   list.append(row);
  }
 }catch{if(stale())return;desktopDraftSignature='';list.replaceChildren(node('p','detect-error','无法读取草稿快照列表；未清理或覆盖任何快照，请检查本机快照目录及权限。'));}
}
// 任务交付详情独立于轮询列表；只读明确请求，不自动执行模型或认领目录文件。
let taskDeliveryGeneration=0;
function clearTaskDelivery(){
 taskDeliveryGeneration++;
 const panel=$('task-delivery-detail');if(panel)panel.remove();
}
async function showTaskDelivery(t){
 clearTaskDelivery();const generation=taskDeliveryGeneration;
 const data=await api().GetTaskDelivery(t.thread_id,t.id);
 if(page!=='tasks'||generation!==taskDeliveryGeneration)return;
 const tasks=$('task-list');if(!tasks)return;
 const panel=node('article','panel');panel.id='task-delivery-detail';
 panel.append(node('h3','','本次任务交付'),node('strong','',t.name||data.thread_id),node('code','',data.event_id));
 panel.append(node('p','',data.delivery==='sent'?'原回复：已确认送达':data.delivery==='pending'?'原回复：尚未全部确认送达；报告可查看，不代表原回复已送达':'原回复：送达结果待核对；查看报告不会将其改为已送达'));
 panel.append(node('h4','','结果摘要（最终回复原文摘录）'),node('p','',data.summary));
 panel.append(node('h4','','验证说明'),node('p','muted',data.verification));
 panel.append(node('h4','','本次报告'),node('p','',data.report_name),node('p','muted','由本次已封存的助手回复生成，不扫描项目目录，不保证模型提及的文件由本任务生成。报告发送不改变原任务或原回复状态。'));
 const details=node('details');details.append(node('summary','','查看完整报告'));
 const content=node('pre');content.style.whiteSpace='pre-wrap';content.style.overflowWrap='anywhere';content.style.maxHeight='420px';content.style.overflow='auto';content.textContent=data.report;details.append(content);panel.append(details);
 const save=node('button','btn small','保存报告到本机');
 save.onclick=()=>task(async()=>{
  if(!await confirmAction('保存本次任务报告？','报告包含这次回复的文本，将保存为本机Markdown文件。请选择可信目录；云同步软件可能上传其中内容。不会覆盖已有文件或执行模型。'))return;
  const path=await api().ExportTaskReport(data.thread_id,data.event_id,true);
  if(path)note('本次报告已保存到所选本机文件；原任务未重新执行。');
 });
 const send=node('button','btn small','发送报告到原会话群');send.id='task-delivery-send';send.disabled=status.phase!=='running';
 send.onclick=()=>task(async()=>{
  if(status.phase!=='running')throw new Error('后台桥接未运行，报告未发送');
  if(!await confirmAction('分享本次任务报告？',`请求：${data.event_id}\n会话：${t.name||data.thread_id}\n报告将发送到该会话当前有效的绑定群，群成员均可见。不会重新执行问题；已有发送记录不会重复上传，未知结果需先人工核对。`))return;
  await api().SendTaskReport(data.thread_id,data.event_id,true);
  note('报告已确认发送，或此前已送达未重复发送。请到对应群查看；原任务未重跑。');await refreshTaskCenter();
 });
 const close=node('button','btn small','收起交付详情');close.onclick=clearTaskDelivery;
 panel.append(save,send,close);const host=tasks.closest('article')||tasks.parentElement;host.before(panel);panel.scrollIntoView({block:'start',behavior:'smooth'});
}
function appendTaskDeliveryButton(row,t){
 if(!t.thread_id||t.result?.execution!=='completed')return;
 const view=node('button','btn small','查看本次交付');
 view.onclick=()=>task(()=>showTaskDelivery(t));row.append(view);
}
async function refreshTaskSection(kind){
 const result=await loadHistoryPage(kind);if(!result)return;
 const ids={tasks:'task-list',deliveries:'delivery-list',creations:'creation-list',attachments:'attachment-list'};
 const list=$(ids[kind]);if(!list)return;list.replaceChildren();appendHistoryPager(list,result,()=>refreshTaskSection(kind));
 if(kind==='tasks'){
  renderWaitingSummary(list,result.waiting);
  for(const t of result.tasks||[]){const row=node('div','closure-row');row.append(node('strong','',t.name||t.thread_id||'历史映射'),node('p','',`${taskStatusNames[t.status]||t.status} · ${taskStageNames[t.stage]||t.stage} · ${new Date(t.received_at*1000).toLocaleString()}`),node('code','',t.id));if(t.waiting_hint&&t.status==='processing')row.append(node('p','muted',t.waiting_hint));if(t.error)row.append(node('p','detect-error',t.error));renderRequestResult(row,t.result);appendTaskDeliveryButton(row,t);list.append(row);}
  if(!(result.tasks||[]).length)list.append(node('p','muted','本页没有任务历史；已清理的终态记录不再显示。'));
 }else if(kind==='deliveries'){
  for(const d of result.deliveries||[]){const row=node('div','closure-row');row.append(node('strong','',d.name||d.thread_id),node('p','',`${d.state} · 片段 ${d.part+1}`),node('code','',d.item_id));if(d.error)row.append(node('p','detect-error',d.error));
   const action=(label,value,confirm)=>{const btn=node('button','btn small',label);btn.disabled=status.phase!=='running';btn.onclick=()=>task(async()=>{if(confirm&&!await confirmAction(label,'此操作只处理当前回复片段，不重新执行模型。请先检查飞书群：确认未收到后重试仍可能重复；已送达请选标记送达。'))return;await api().ResolveDelivery({thread_id:d.thread_id,item_id:d.item_id,part:d.part,action:value,confirmed:!!confirm});await refreshTaskSection('deliveries');await refreshTaskSection('tasks');});row.append(btn);};
   if(d.state==='pending')action('仅重试投递','retry',false);
   if(d.state==='unknown'){action('已核实收到，标记送达','confirmed_sent',true);action('已核实未收到，重试','confirmed_missing',true);}list.append(row);
  }
  if(!(result.deliveries||[]).length)list.append(node('p','muted','没有待处理的回复投递。'));
 }else if(kind==='creations'){
  for(const f of result.creations||[]){const row=node('div','closure-row');row.append(node('strong','',f.thread_id||'尚未创建会话'),node('p','',f.state+' · '+f.stage));if(f.error)row.append(node('p','detect-error',f.error));list.append(row);}
  if(!(result.creations||[]).length)list.append(node('p','muted','本页没有创建历史；发送 /new 可获取一次性创建表单。'));
 }else{
  for(const f of result.attachments||[]){const row=node('div','closure-row');row.append(node('strong','',f.name),node('p','',`${f.direction==='in'?'接收':'发送'} · ${f.kind} · ${transferNames[f.state]||f.state} · ${new Date(f.updated_at*1000).toLocaleString()}`));if(f.error)row.append(node('p','detect-error',f.error));list.append(row);}
  if(!(result.attachments||[]).length)list.append(node('p','muted','本页没有附件记录。'));
 }
}
let taskTargetsSignature='',taskTargetsGeneration=0;
async function refreshTaskTargets(){
 const generation=++taskTargetsGeneration;let targets;try{targets=await api().GetTaskTargets();}catch(error){if(generation!==taskTargetsGeneration)return;throw error;}if(generation!==taskTargetsGeneration||page!=='tasks')return;const signature=JSON.stringify(targets);
 if(signature===taskTargetsSignature)return;
 taskTargetsSignature=signature;fileChoices=targets;
 const fields=$('file-send-fields');fields.replaceChildren();
 if(!fileChoices.some(p=>p.thread_id===fileThread))fileThread=fileChoices[0]?.thread_id||'';
 bindValue(fields,'目标会话',fileThread,v=>fileThread=v,{options:fileChoices.map(p=>[p.thread_id,p.name||p.thread_id])});
 bindValue(fields,'发送类型',fileKind,v=>fileKind=v,{options:[['file','文件'],['image','图片（PNG/JPEG/GIF）']]});
}
let taskCenterGeneration=0;
async function refreshTaskCenter(){
 const refreshGeneration=++taskCenterGeneration;
 const ids={tasks:'task-list',deliveries:'delivery-list',creations:'creation-list',attachments:'attachment-list'};
 const sections=['tasks','deliveries','creations','attachments'];
 const jobs=sections.map(kind=>({id:ids[kind],run:()=>refreshTaskSection(kind),version:()=>historyPage(kind).generation}));
 jobs.push({id:'desktop-draft-list',run:refreshDesktopDrafts,version:()=>historyPage('drafts').generation},{id:'file-send-fields',run:refreshTaskTargets,version:()=>taskTargetsGeneration});
 await Promise.allSettled(jobs.map(async job=>{
  const box=$(job.id);box?.querySelectorAll('.history-read-error').forEach(el=>el.remove());
  const pending=job.run(),generation=job.version();
  try{await pending;}catch{
   // 错误也在本请求自己的完成时处理，并复核该区块代次，不等其它区块结束。
   if(generation!==job.version()||page!=='tasks'||!box)return;
   if(job.id==='file-send-fields'){fileChoices=[];fileThread='';taskTargetsSignature='';box.replaceChildren();}
   box.prepend(node('p','detect-error history-read-error','此区域暂时读取失败，请刷新重试；其它列表和草稿恢复不受影响，原有内容可能已过时。'));
  }
 }));
 if(refreshGeneration===taskCenterGeneration)taskCenterAt=Date.now();
}
$('new-form').onclick=()=>task(async()=>{if(!fileThread)throw new Error('请先选择表单发送入口');await api().OpenCreationForm(fileThread);note('已向所选群发送新建会话表单，请在飞书里选择并提交。');await refreshTaskCenter();});
$('refresh-tasks').onclick=()=>task(refreshTaskCenter);
$('send-file').onclick=()=>task(async()=>{
 if(!fileThread)throw new Error('请先选择活跃会话');if(status.phase!=='running')throw new Error('请先启动后台桥接');
 const path=await api().ChooseAttachment();if(!path)return;
 const target=fileChoices.find(p=>p.thread_id===fileThread);
 if(!await confirmAction('确认分享文件？',`发送到「${target?.name||fileThread}」群的所有成员：${path}。只允许会话工作目录内文件，文件内容不会自动执行。`))return;
 $('file-send-status').textContent='上传和发送中，请勿重复点击…';
 try{await api().SendFile({id:crypto.randomUUID(),thread_id:fileThread,path,kind:fileKind});$('file-send-status').textContent='平台已确认发送，请到对应群查看。';}catch(e){$('file-send-status').textContent='发送未确认或失败，请先查看附件记录和群消息，不要直接重发。';throw e;}finally{await refreshTaskCenter();}
});
refreshBackground().catch(()=>{});

$('session-status').onclick=()=>task(async()=>{if(!fileThread)throw new Error('请先选择会话群或话题');await api().OpenSessionStatus(fileThread);note('已请求显示或刷新会话小卡片；仅显示卡片不会启动或中断任务。');});


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
