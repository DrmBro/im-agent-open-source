/* 消息助理：今日事项分流、关注对象设置、回复确认与会话派发。数据由后台消息助理生成。 */
'use strict';
(() => {
 const el=id=>document.getElementById(id);
 const call=r=>api().Assistant(r);
 const errorText=e=>String(e?.message||e);
 const clock=ms=>ms?new Date(ms).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'}):'';
 const categories={all:'全部',need_me:'需要我处理',auto_reply:'已有结论',no_reply:'无需回复',awaiting:'等待对方回复',session:'已派发会话',closed:'已完成 / 忽略'};
 const hints={all:'今天的全部事项',need_me:'需要你决策或亲自处理',auto_reply:'查看助理给你的结论',no_reply:'通知、知会、他人对话',awaiting:'你问了别人、对方还没答复',session:'复杂事项由独立会话处理',closed:'已发送、已处理或忽略'};
 const replyStates={sending:'发送中',sent:'已发送',unknown:'送达待核对',failed:'发送失败'};
 let view=null,filter='need_me',signature='',refreshAt=0,pending=false,generation=0,editing=null,expanded=new Set();

 const current=()=>page==='assistant';
 const ago=ms=>{if(!ms)return '';const m=Math.max(0,Math.round((Date.now()-ms)/60000));return m<60?m+' 分钟':m<1440?Math.floor(m/60)+' 小时':Math.floor(m/1440)+' 天';};
 // 助理设置独立保存，不标记主配置为“未保存”。
 function field(parent,label,value,set,{help='',type='text',options=null}={}){
  const f=node('div','field'),lab=node('label','',label),input=node(options?'select':'input');input.id='assistant-'+crypto.randomUUID();lab.htmlFor=input.id;
  if(options)for(const [v,t] of options)input.append(new Option(t,v));else input.type=type;
  input.value=value??'';input.addEventListener(options?'change':'input',()=>set(input.value));f.append(lab,input);if(help)f.append(node('small','',help));parent.append(f);return input;
 }
 function check(parent,label,value,set,help=''){
  const f=node('div','field'),row=node('div','toggle-line'),input=node('input'),lab=node('label','',label);input.type='checkbox';input.checked=!!value;input.id='assistant-'+crypto.randomUUID();lab.htmlFor=input.id;input.onchange=()=>set(input.checked);row.append(input,lab);f.append(row);if(help)f.append(node('small','',help));parent.append(f);
 }
 function show(error){const box=el('assistant-error');box.hidden=!error;box.textContent=error?('操作失败：'+errorText(error)):'';}
 function bucket(x){if(x.awaiting&&x.state!=='ignored')return 'awaiting';if(x.state==='session')return 'session';if(x.state==='done'||x.state==='ignored')return x.category==='no_reply'&&x.state==='done'?'no_reply':'closed';return x.category;}
 function countBy(){const c={all:0,need_me:0,auto_reply:0,no_reply:0,awaiting:0,session:0,closed:0};for(const x of view?.items||[]){c.all++;c[bucket(x)]++;}return c;}

 function renderStatus(){
  const s=el('assistant-status');s.replaceChildren();
  const set=view?.settings||{};
  const text=set.enabled?(view.status||'等待首轮拉取'):'消息助理未开启，请在设置中开启并选择关注的人和群。';
  s.append(node('span','dot'+(set.enabled?' on':'')),node('span','',text));
  if(view?.last_run)s.append(node('span','muted','上次运行 '+new Date(view.last_run*1000).toLocaleTimeString()));
  if(view?.pending)s.append(node('span','chip','待分析 '+view.pending+' 条'));
  if(view.match_stats)s.append(node('span','muted',view.match_stats));
  if(set.model)s.append(node('span','chip accent','模型 '+set.model));
 }

 function renderStats(){
  const box=el('assistant-stats'),counts=countBy();box.replaceChildren();
  for(const [key,name] of Object.entries(categories)){
   const b=node('button','assistant-stat assistant-'+key);b.type='button';b.setAttribute('aria-pressed',String(filter===key));b.classList.toggle('selected',filter===key);
   b.append(node('span','assistant-stat-label',name),node('strong','',String(counts[key]||0)),node('small','',hints[key]));
   b.onclick=()=>{filter=key;render(true);};box.append(b);
  }
 }

 function action(label,cls,fn,confirm){const b=node('button','btn small '+(cls||''),label);b.type='button';b.onclick=()=>task(async()=>{if(confirm&&!await confirmAction(confirm.title,confirm.message,{confirmText:label,danger:confirm.danger}))return;try{await fn();show('');}catch(e){show(e);throw e;}await refresh(true);});return b;}

 // 内部分析始终只读；只有明确选择整理时才生成钉钉正文。
 function externalDraft(parent,request,send){
  const editor=node('div','assistant-external');
  const open=node('button','btn small','整理钉钉回复');open.type='button';
  open.onclick=()=>task(async()=>{
   try{
    const result=await call({...request,action:'compose'});
    const field=node('textarea','assistant-reply');field.rows=4;field.value=result.text;field.maxLength=4000;
    field.setAttribute('aria-label','钉钉发送正文');
    const deliver=action('发送到钉钉','primary',async()=>{await send(field.value);editor.replaceChildren();});
    const cancel=node('button','btn small','取消');cancel.type='button';cancel.onclick=()=>editor.replaceChildren();
    editor.replaceChildren(field,deliver,cancel);requestAnimationFrame(()=>field.focus());
   }catch(e){show(e);}
  });
  parent.append(open,editor);
 }
 function renderItem(x){
  const card=node('article','assistant-item assistant-'+bucket(x));
  const head=node('div','assistant-item-head');
  head.append(node('span','chip'+(x.kind==='group'?'':' accent'),x.kind==='group'?'群聊':'单聊'),node('strong','',x.title));
  if(x.complex)head.append(node('span','chip warn','复杂'));
  if(x.awaiting)head.append(node('span','chip'+(x.overdue?' danger':' accent'),(x.overdue?'已超时 · ':'')+'等'+(x.awaiting_who||'对方')+'回复 '+ago(x.awaiting_since)));
  head.append(node('time','',clock(x.updated_ms)));
  card.append(head,node('p','assistant-chat',x.chat_name||x.conversation));
  if(x.summary)card.append(node('p','assistant-summary',x.summary));
  if(x.reason)card.append(node('p','muted','判断：'+x.reason));
  const msgs=node('details','assistant-messages');msgs.open=expanded.has(x.id);msgs.ontoggle=()=>{if(msgs.open)expanded.add(x.id);else expanded.delete(x.id);};
  msgs.append(node('summary','',`相关消息 ${x.messages?.length||0} 条`));
  for(const m of x.messages||[]){const row=node('div','assistant-message'+(m.self?' self':''));row.append(node('span','assistant-sender',(m.self?'我':m.sender_name||'未知')+' · '+clock(m.created_ms)),node('span','assistant-text',m.text));if(m.quoted)row.append(node('span','muted','引用：'+m.quoted));msgs.append(row);}
  card.append(msgs);
  const actions=node('div','assistant-actions');
  if(x.category==='auto_reply'&&x.state!=='session'){
   card.append(node('p','assistant-original',x.reply||''));
   if(x.reply_state)card.append(node('p',x.reply_state==='sent'?'muted':'notice warning',(replyStates[x.reply_state]||x.reply_state)+(x.reply_error?' · '+x.reply_error:'')+(x.reply_receipt?' · 回执 '+x.reply_receipt:'')));
   if(x.sent_body)card.append(node('p','assistant-original','对外正文 · '+x.sent_body));
   if(!x.reply_state||x.reply_state==='failed')externalDraft(card,{id:x.id},text=>call({action:'reply',id:x.id,text}));
   if(x.reply_state==='unknown')actions.append(action('已核对未送达，允许重发','',()=>call({action:'reply_reset',id:x.id}),{title:'确认未送达？',message:'请先到钉钉确认对方没有收到，否则重发可能造成重复。'}));
  }
  if(x.awaiting){
   const box=node('textarea','assistant-reply');box.rows=2;box.maxLength=4000;box.placeholder='催办内容，例如：方便的话今天给我一下进度？';box.value=editing?.id===x.id+':f'?editing.text:'';box.oninput=()=>{editing={id:x.id+':f',text:box.value};};box.setAttribute('aria-label','催办内容');card.append(box);
   actions.append(action('发送催办','primary',async()=>{if(!box.value.trim())throw new Error('请填写催办内容');await call({action:'followup',id:x.id,text:box.value});editing=null;},{title:'发送催办？',message:`将以你的钉钉身份发送到「${x.chat_name}」，并重新开始计时：\n\n${box.value.slice(0,500)}`}));
   actions.append(action('不用等了','',()=>call({action:'unawait',id:x.id})));
  }else if(x.state!=='ignored')actions.append(action('标记为等待回复','',()=>call({action:'await',id:x.id})));
  if(x.state==='session'){
   card.append(node('p','notice','已派发会话 '+x.thread_id.slice(0,12)+(x.queued?`，队列中有 ${x.queued} 条新消息等待会话读取`:'')));
   if(x.queued)actions.append(action('合并补充到原会话','primary',()=>call({action:'supplement',id:x.id,through:x.queue_through})));
   for(const batch of view.batches||[]){if(batch.item!==x.id)continue;
    const record=node('details');record.append(node('summary','','补充 '+batch.message_ids.length+' 条 · '+batch.state));
    for(const id of batch.message_ids){const m=(x.messages||[]).find(v=>v.message_id===id);record.append(node('p','muted',m?clock(m.created_ms)+' · '+m.sender_name+' · '+m.text:id));}
    card.append(record);
   }
  }else if(x.state==='open'){
   if(x.session_error)card.append(node('p','notice warning','派发会话失败：'+x.session_error));
   actions.append(action(x.session_error?'重试派发会话':'派发会话处理','',()=>call({action:'session',id:x.id})));
   actions.append(action('标记已处理','',()=>call({action:'done',id:x.id})));
   actions.append(action('忽略','',()=>call({action:'ignored',id:x.id})));
  }else{
   actions.append(action('重新打开','',()=>call({action:'open',id:x.id})));
  }
  if(x.state!=='session'){
   const move=node('select','assistant-move');move.setAttribute('aria-label','纠正分类');move.append(new Option('纠正分类…',''));
   for(const k of ['need_me','auto_reply','no_reply'])if(k!==x.category)move.append(new Option('改为「'+categories[k]+'」',k));
   move.onchange=()=>{const to=move.value;if(!to)return;task(async()=>{try{await call({action:'recategorize',id:x.id,category:to});note('已改分类，助理会参考你的纠正。');}catch(e){show(e);}await refresh(true);});};
   actions.append(move);
  }
  card.append(actions);
  return card;
 }

 function renderList(){
  const list=el('assistant-list');list.replaceChildren();
  const rows=(view?.items||[]).filter(x=>filter==='all'||bucket(x)===filter);
  for(const x of rows)list.append(renderItem(x));
  if(!rows.length)list.append(node('p','board-empty',view?.settings?.enabled?'这个分类今天还没有事项。':'开启消息助理后，这里会显示今天的事项。'));
 }

 function renderReviews(){
  const box=el('assistant-reviews');box.replaceChildren();
  const rows=view?.reviews||[];if(!rows.length)return;
  const panel=node('details','panel assistant-review-panel');panel.open=true;panel.append(node('summary','',`会话分析结果（${rows.length}）`));
  for(const r of rows){
   const card=node('div','assistant-item');
   card.append(node('p','muted',(r.peer_name||r.peer)+' · '+(r.target?.includes(':group:')?'群聊':'单聊')+' · 会话 '+r.thread_id.slice(0,12)));
   card.append(node('pre','assistant-original',r.input||''));
   card.append(node('p','assistant-original',r.draft||''));
   if(r.reason)card.append(node('p','muted',r.reason));
   if(['approved','sending','sent'].includes(r.state)){
    card.append(node('p','muted',{approved:'已确认，等待发送',sending:'发送中',sent:'已确认送达'}[r.state]),node('p','assistant-original',r.approved_text||''));
    if(r.receipts)card.append(node('p','muted','回执 '+r.receipts));
    panel.append(card);continue;
   }
   if(r.state==='unknown'){card.append(node('p','notice warning','发送结果不确定，请先到钉钉核对；不会自动重发。'));panel.append(card);continue;}
   const actions=node('div','assistant-actions');
   const review=(kind,text='')=>call({action:'review',review:{event_id:r.event_id,version:r.version,action:kind,text}});
   if(r.can_send)externalDraft(card,{review:{event_id:r.event_id}},text=>review('send',text));
   actions.append(action('我已自行回复','',()=>review('manual')),action('忽略','',()=>review('ignored')));
   card.append(actions);panel.append(card);
  }
  box.append(panel);
 }

 function renderCursors(){
  const box=el('assistant-cursors');box.replaceChildren();
  for(const c of view?.cursors||[])box.append(node('p',c.error?'notice warning':'muted',`${c.name||c.conversation} · ${c.kind==='group'?'群':c.kind==='direct'?'单聊':'@我'} · 读到 ${clock(c.since_ms)} · ${new Date(c.pulled_at*1000).toLocaleTimeString()}${c.error?' · '+c.error:''}`));
  if(!box.children.length)box.append(node('p','muted','今天还没有拉取记录。'));
 }

 function targetList(title,list,{search,placeholder,recent}){
  const wrap=node('div','field');wrap.append(node('span','',title));
  const chips=node('div','assistant-chips');
  const draw=()=>{chips.replaceChildren();for(const t of list){const c=node('span','chip accent',t.name||t.id);const x=node('button','chip-remove','×');x.type='button';x.setAttribute('aria-label','移除 '+(t.name||t.id));x.onclick=()=>{list.splice(list.indexOf(t),1);draw();};c.append(x);chips.append(c);}if(!list.length)chips.append(node('span','muted','尚未添加'));};
  draw();wrap.append(chips);
  const row=node('div','assistant-search'),input=node('input'),go=node('button','btn small','搜索'),results=node('div','assistant-results');input.type='search';input.placeholder=placeholder;go.type='button';row.append(input,go);
  const pick=t=>{if(!list.some(x=>x.id===t.id))list.push({id:t.id,name:t.name});draw();};
  let request=0;
  const run=async fn=>{const version=++request;results.replaceChildren(node('p','muted','正在读取…'));try{const rows=await fn();if(version!==request)return;results.replaceChildren();for(const r of rows){const b=node('button','btn small',r.name+(r.title?' · '+r.title:''));b.type='button';b.onclick=()=>pick(r);results.append(b);}if(!rows.length)results.append(node('p','muted','没有结果'));}catch(e){if(version===request)results.replaceChildren(node('p','notice warning',errorText(e)));}};
  if(recent){const r=node('button','btn small','今天活跃的群');r.type='button';r.onclick=()=>run(recent);row.append(r);}
  go.onclick=()=>run(()=>search(input.value));input.onkeydown=e=>{if(e.key==='Enter'){e.preventDefault();go.click();}};
  wrap.append(row,results);return wrap;
 }

 function renderSettings(){
  const box=el('assistant-settings');box.replaceChildren();
  const s=structuredClone(view?.settings||{});s.people||=[];s.groups||=[];
  box.append(node('h3','','消息助理设置'),node('p','muted','设置保存在本机业务数据库，保存后立即生效，无需重启桥接。'));
  const grid=node('div','form-panel');
  check(grid,'开启消息助理',!!s.enabled,v=>s.enabled=v,'定时拉取今天（上海时间零点起）的新消息；更早的消息不会读取。');
  const conns=(snapshot?.config?.connections||[]).filter(c=>c.type==='dingtalk_personal');
  field(grid,'钉钉个人账号连接',s.connection||'',v=>s.connection=v,{options:[['','自动选择'],...conns.map(c=>[c.id,c.id])],help:'需要先在“IM 连接”完成 DWS 登录。'});
  const models=[['','跟随 Codex 当前模型'],...(view?.models||[]).map(m=>[m,m])];if(s.model&&!models.some(m=>m[0]===s.model))models.push([s.model,s.model]);
  field(grid,'分析模型',s.model||'',v=>s.model=v,{options:models,help:'模型列表读取自 CC Switch 写入的 Codex 目录，直接调用当前供应商；切换 CC Switch 供应商后自动生效。'});
  field(grid,'拉取间隔（秒）',String(s.interval_seconds||120),v=>s.interval_seconds=Number(v)||120,{type:'number',help:'30 至 3600 秒。收到实时消息时也会提前拉取。'});
  field(grid,'等待回复超时（小时）',String(s.await_hours||4),v=>s.await_hours=Number(v)||4,{type:'number',help:'你问了别人、超过这个时间对方还没答复时，发一次本机通知并标红。等待中的会话跨天继续拉取（最多 7 天）。'});
  check(grid,'包含今天 @我 的群',s.mentions!==false,v=>s.mentions=v,'自动把今天 @你 的群加入拉取范围。');
  check(grid,'跳过机器人消息',s.skip_robots!==false,v=>s.skip_robots=v,'告警、通知类机器人消息不进入分析。');
  check(grid,'复杂事项自动派发会话',s.auto_session!==false,v=>s.auto_session=v,'“需要我处理”且需要多步工作的事项，优先复用 Codex 客户端正在处理同一事项的会话，没有匹配时创建新会话；分析结果先给你查看。');
  check(grid,'会话空闲时提醒读取新消息',!!s.auto_deliver_idle,v=>s.auto_deliver_idle=v,'已派发会话的事项有新消息时只进队列；开启后，会话空闲时追加一条提醒让它去读队列。会话执行中不会被打断。');
  box.append(grid);
  box.append(targetList('关注的人（单聊）',s.people,{placeholder:'输入姓名搜索通讯录',search:async q=>(await call({action:'search_people',query:q})).people||[]}));
  box.append(targetList('关注的群',s.groups,{placeholder:'输入群名搜索我加入的全部群',search:async q=>(await call({action:'search_groups',query:q})).groups||[],recent:async()=>(await call({action:'search_groups'})).groups||[]}));
  const rules=node('div','form-panel');
  for(const [key,label,help] of [['profile','我的背景（可选）','例如职责、负责的系统、常见同事，帮助判断哪些需要你处理。'],['rule_need_me','“需要我处理”补充规则',''],['rule_auto_reply','“已有结论”补充规则',''],['rule_no_reply','“无需回复”补充规则','']]){
   const f=node('div','field'),t=node('textarea');f.append(node('span','',label),t);t.rows=2;t.maxLength=2000;t.value=s[key]||'';t.oninput=()=>s[key]=t.value;if(help)f.append(node('small','',help));rules.append(f);
  }
  box.append(node('h4','','分流规则（自然语言）'),rules);
  const save=node('button','btn primary','保存设置');save.type='button';
  save.onclick=()=>task(async()=>{try{view=(await call({action:'save',settings:s})).view;show('');note('消息助理设置已保存。');signature='';render(true);}catch(e){show(e);throw e;}});
  box.append(save);
 }

 function render(force){
  if(!view)return;
  renderStatus();
  const next=JSON.stringify([view.items,view.reviews,view.batches,view.cursors,filter]);
  if(!force&&next===signature)return;
  if(!force&&document.querySelector('.assistant-external textarea'))return;
  if(!force&&document.activeElement?.classList?.contains('assistant-reply'))return; // 正在编辑回复时不重绘列表
  signature=next;
  renderStats();renderReviews();renderList();renderCursors();
 }

 async function refresh(force=false){
  if(!current()||(pending&&!force))return;
  const request=++generation;pending=true;
  try{const r=await call({action:'view'});if(request!==generation||!current())return;view=r.view;el('assistant-error').hidden=true;render(force);}
  catch(e){if(request===generation&&current()){const box=el('assistant-error');box.hidden=false;box.textContent='读取失败：'+errorText(e);}}
  finally{if(request===generation){pending=false;refreshAt=Date.now();}}
 }

 el('assistant-run').onclick=()=>task(async()=>{await call({action:'run'});note('已触发拉取，稍后自动刷新。');setTimeout(()=>refresh(true),4000);});
 el('assistant-settings-toggle').onclick=()=>{const box=el('assistant-settings');box.hidden=!box.hidden;if(!box.hidden)renderSettings();};
 window.assistantPage={enter:async()=>{signature='';await refresh(true);if(view&&!view.settings?.enabled&&el('assistant-settings').hidden){el('assistant-settings').hidden=false;renderSettings();}},leave:()=>{generation++;pending=false;},poll:async()=>{if(Date.now()-refreshAt>5000)await refresh();}};
})();
