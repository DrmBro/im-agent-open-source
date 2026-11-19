/* 今日个人消息收件箱；只在用户点击后创建会话或忽略，不自动处理。 */
'use strict';
(() => {
 const list=()=>document.getElementById('personal-inbox-list'),error=()=>document.getElementById('personal-inbox-error');
 let generation=0,pending=false,at=0,items=[],signature=null;
 const current=()=>page==='personal-inbox';
 const text=e=>String(e?.message||e);
 const time=s=>new Date(s*1000).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
 const filter=document.getElementById('inbox-filter');
 const opened=new Set(),openedTopics=new Set(),selection=new Map();
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
 function startSession(ids){
  return task(async()=>{
   await api().ReviewPersonalInbox({action:'start',ids});
   note(ids.length===1?'已用这条消息和它的上下文创建会话。':'已用所选消息和上下文创建会话。');
   await refresh(true);
  });
 }
 function startButton(label,rows){
  const button=node('button','btn small',label);
  button.type='button';
  button.onclick=()=>startSession(rows.map(x=>x.id));
  return button;
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
  row.append(startButton('单独创建会话',[x]));
  return row;
 }
 function groupTopics(rows){
  const groups=new Map();
  for(const x of rows){
   const key=x.topic_id||x.id;
   if(!groups.has(key))groups.set(key,[]);
   groups.get(key).push(x);
  }
  return Array.from(groups.values());
 }
 function topicCard(rows,onSelection){
  const first=rows[0],card=node('article','inbox-topic');
  const details=node('details'),head=node('summary','inbox-summary');
  const needsAttention=rows.some(attention);
  head.append(node('strong','',first.topic_title),node('span','muted',rows.length+' 条'));
  if(needsAttention)head.append(node('span','chip accent','需关注'));
  head.append(node('time','muted',time(first.received)));
  details.open=openedTopics.has(first.topic_id);
  details.append(head);
  const body=node('div');
  details.append(body);
  function fill(){
   if(!body.childElementCount)for(const x of rows)body.append(messageRow(x,onSelection));
  }
  if(details.open)fill();
  details.ontoggle=()=>{
   if(!details.isConnected)return;
   if(details.open){openedTopics.add(first.topic_id);fill();}
   else openedTopics.delete(first.topic_id);
  };
  card.append(details,startButton('处理此事项（'+rows.length+' 条）',rows));
  return card;
 }
 function conversation(key,rows,shown){
  const latest=shown[0],row=node('details','inbox-conversation');
  const title=latest.is_direct?(latest.user_name||latest.user):(latest.chat_name||'群聊 · '+latest.chat.slice(-12));
  const head=node('summary','inbox-summary');
  const count=rows.filter(attention).length;
  head.append(node('strong','',title),node('span','chip',latest.is_direct?'单聊':'群聊'),
   node('span','muted',rows.length+' 条'),node('span',count?'chip accent':'muted',count?'需关注 '+count+' 条':''),
   node('time','muted',time(latest.received)));
  if(shown.every(x=>x.topic_id))head.insertBefore(node('span','chip',groupTopics(shown).length+' 件事'),head.lastChild);
  row.open=opened.has(key);
  row.append(head);
  const content=node('div','inbox-conversation-body');
  row.append(content);
  function showMessages(){
   content.replaceChildren();
   const create=node('button','btn primary small');
   create.type='button';
   const ids=()=>shown.filter(selected).map(x=>x.id);
   const update=()=>{const n=ids().length;create.disabled=!n;create.textContent=n?'合并勾选消息（'+n+' 条）':'勾选消息以创建会话';};
   create.onclick=()=>startSession(ids());
   update();
   content.append(create);
   if(shown.length>=2&&shown.length<=24){
    const choices=node('select');choices.setAttribute('aria-label','重新归并当前显示的消息');
    choices.append(node('option','','调整事项数…'));
    for(let n=1;n<=shown.length;n++){
     const option=node('option','','归并为 '+n+' 件');option.value=String(n);choices.append(option);
    }
    choices.onchange=()=>{
     const count=Number(choices.value);choices.selectedIndex=0;
     if(!count)return;
     task(async()=>{await api().ReviewPersonalInbox({action:'regroup',ids:shown.map(x=>x.id),count});await refresh(true);});
    };
    content.append(choices);
   }
   for(const messages of groupTopics(shown)){
    content.append(messages[0].topic_id?topicCard(messages,update):messageRow(messages[0],update));
   }
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
  const conversations=[];
  for(const [key,rows] of groups){
   const shown=rows.filter(visible);
   if(shown.length)conversations.push({key,rows,shown});
  }
  conversations.sort((a,b)=>b.shown[0].received-a.shown[0].received||a.key.localeCompare(b.key));
  for(const {key,rows,shown} of conversations)fragment.append(conversation(key,rows,shown));
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
