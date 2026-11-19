/* 今日个人消息收件箱；只在用户点击后创建会话或忽略，不自动处理。 */
'use strict';
(() => {
 const list=()=>document.getElementById('personal-inbox-list'),error=()=>document.getElementById('personal-inbox-error');
 let generation=0,pending=false,at=0,items=[],signature=null;
 const current=()=>page==='personal-inbox';
 const text=e=>String(e?.message||e);
 const time=s=>new Date(s*1000).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
 const filter=document.getElementById('inbox-filter');
 const opened=new Set(),selection=new Map();
 const ready=x=>Boolean(x.analysis&&!x.analysis.failed);
 const attention=x=>ready(x)&&x.analysis.attention;
 const selected=x=>selection.has(x.id)?selection.get(x.id):attention(x);
 function visible(x){
  switch(filter.value){
   case 'attention':return attention(x);
   case 'session':return ready(x)&&x.analysis.new_session;
   case 'ready':return ready(x);
   case 'other':return ready(x)&&!x.analysis.attention;
   case 'pending':return !x.analysis;
   case 'failed':return Boolean(x.analysis?.failed);
   default:return true;
  }
 }
 function messageRow(x,onSelection){
  const row=node('article','inbox-message'),head=node('div','inbox-head');
  const choice=node('label','inbox-choice'),check=node('input');
  check.type='checkbox';check.checked=selected(x);
  check.onchange=()=>{selection.set(x.id,check.checked);onSelection();};
  choice.append(check,node('strong','',x.user_name||x.user));
  head.append(choice,node('time','',time(x.received)));
  const ignore=node('button','btn small','忽略');ignore.type='button';
  ignore.onclick=()=>task(async()=>{
   if(!await confirmAction('忽略这条消息？',(x.text||'').slice(0,200),{confirmText:'忽略'}))return;
   await api().ReviewPersonalInbox({id:x.id,action:'ignore'});await refresh(true);
  });
  head.append(ignore);
  row.append(head,node('p','inbox-text',x.text||'（无文字）'));
  const a=x.analysis;
  if(!a)row.append(node('p','muted','等待模型分析'));
  else if(a.failed)row.append(node('p','notice warning',a.reason));
  else {
   row.append(node('strong','',a.attention?'需要关注':'无需特别关注'),node('span','chip',a.new_session?'建议新建会话':'无需新建会话'),node('p','',a.reason));
   if(a.completed_request)row.append(node('p','inbox-text','补全后的请求：'+a.completed_request));
   if(a.evidence?.length){
    const details=node('details');details.append(node('summary','','查看补全依据'));
    for(const excerpt of a.evidence)details.append(node('p','inbox-text',excerpt));
    row.append(details);
   }
   if(a.note)row.append(node('p','muted',a.note));
  }
  return row;
 }
 function conversation(key,rows,shown){
  const latest=rows[0],row=node('details','inbox-conversation');
  const title=latest.is_direct?(latest.user_name||latest.user):(latest.chat_name||'群聊 · '+latest.chat.slice(-12));
  const head=node('summary','inbox-summary');
  const count=rows.filter(attention).length;
  head.append(node('strong','',title),node('span','chip',latest.is_direct?'单聊':'群聊'),
   node('span','muted',rows.length+' 条'),node('span',count?'chip accent':'muted',count?'需关注 '+count+' 条':''),
   node('time','muted',time(latest.received)));
  row.open=opened.has(key);
  row.append(head);
  const content=node('div','inbox-conversation-body');
  row.append(content);
  function showMessages(){
   content.replaceChildren();
   const create=node('button','btn primary small');
   create.type='button';
   const ids=()=>shown.filter(selected).map(x=>x.id);
   const update=()=>{const n=ids().length;create.disabled=!n;create.textContent=n?'创建会话（'+n+' 条）':'勾选消息以创建会话';};
   create.onclick=()=>task(async()=>{
    await api().ReviewPersonalInbox({action:'start',ids:ids()});
    note('已创建会话并提交所选消息的上下文。');await refresh(true);
   });
   update();
   content.append(create);
   for(const x of shown)content.append(messageRow(x,update));
  }
  if(row.open)showMessages();
  row.ontoggle=()=>{
   if(!row.isConnected)return;
   if(row.open){opened.add(key);if(!content.childElementCount)showMessages();}
   else{opened.delete(key);content.replaceChildren();}
  };
  return row;
 }
 function renderList(){
  const groups=new Map();
  for(const x of items){
   const key=JSON.stringify([x.channel,x.connection,x.is_direct,x.is_direct?x.user:x.chat]);
   if(!groups.has(key))groups.set(key,[]);
   groups.get(key).push(x);
  }
  const box=list(),fragment=document.createDocumentFragment();
  for(const [key,rows] of groups){
   const shown=rows.filter(visible);
   if(shown.length)fragment.append(conversation(key,rows,shown));
  }
  const count=fragment.childElementCount;
  document.getElementById('inbox-analysis-count').textContent=count+' 个对话 · 今日需关注 '+items.filter(attention).length+' 条';
  if(!count)fragment.append(node('p','list-empty',items.length?'没有符合筛选条件的消息':'今天暂无待处理个人消息'));
  box.replaceChildren(fragment);
 }
 filter.onchange=renderList;
 async function refresh(force=false){
  if(!current()||pending&&!force)return;pending=true;const g=++generation;
  const button=document.getElementById('personal-inbox-refresh');if(button)button.disabled=true;
  try{
   const rows=await api().GetPersonalInbox();if(g!==generation||!current())return;
   items=(rows||[]).filter(x=>x.state==='new').sort((a,b)=>b.received-a.received);
   const next=JSON.stringify(items);
   if(force||next!==signature){signature=next;renderList();}
   error().hidden=true;at=Date.now();
  }catch(e){if(g===generation&&current()){error().hidden=false;error().textContent='读取个人消息失败：'+text(e);}}
  finally{if(g===generation){pending=false;if(button)button.disabled=false;}}
 }
 document.getElementById('personal-inbox-refresh')?.addEventListener('click',()=>refresh(true));
 window.personalInbox={enter:()=>refresh(true),leave:()=>{generation++;pending=false;},poll:async()=>{if(Date.now()-at>5000)await refresh();}};
})();
