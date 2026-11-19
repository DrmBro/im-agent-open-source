/* 今日个人消息收件箱；只在用户点击后创建会话或忽略，不自动处理。 */
'use strict';
(() => {
 const list=()=>document.getElementById('personal-inbox-list'),error=()=>document.getElementById('personal-inbox-error');
 let generation=0,pending=false,at=0,onlyAnalyzed=false,items=[];
 const current=()=>page==='personal-inbox';
 const text=e=>String(e?.message||e);
 const time=s=>new Date(s*1000).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
 function card(x){
  const row=node('article','inbox-card'),head=node('div','inbox-head');
  const when=node('time','',time(x.received));when.dateTime=new Date(x.received*1000).toISOString();when.title=new Date(x.received*1000).toLocaleString();
  head.append(node('strong','',x.user_name||x.user),node('span','chip',x.channel),node('span','chip',x.is_direct?'单聊':'群聊'));
  if(x.mention)head.append(node('span','chip accent','@我'));
  if(x.quoted)head.append(node('span','chip','有引用'));
  head.append(when);
  const actions=node('div','inbox-actions'),start=node('button','btn small primary','创建会话并处理'),ignore=node('button','btn small','忽略');
  start.type=ignore.type='button';
  start.onclick=()=>task(async()=>{await api().ReviewPersonalInbox({id:x.id,action:'start'});note('已为这条消息创建会话。');await refresh(true);});
  ignore.onclick=()=>task(async()=>{if(!await confirmAction('忽略这条消息？',`来自 ${x.user_name||x.user}：\n${(x.text||'').slice(0,200)}\n\n忽略后不会创建会话，也不会回复对方。`,{confirmText:'忽略'}))return;await api().ReviewPersonalInbox({id:x.id,action:'ignore'});await refresh(true);});
  actions.append(start,ignore);
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
  row.append(actions);
  return row;
 }
 const filter=document.getElementById('inbox-analyzed-filter');
 function renderList(){
  const ready=items.filter(x=>x.analysis&&!x.analysis.failed);
  const box=list(),visible=onlyAnalyzed?ready:items;
  filter.textContent='已出建议（'+ready.length+'）';
  filter.setAttribute('aria-pressed',String(onlyAnalyzed));
  filter.classList.toggle('primary',onlyAnalyzed);
  document.getElementById('inbox-analysis-count').textContent='今日待处理 '+items.length+' 条';
  box.replaceChildren();
  for(const item of visible)box.append(card(item));
  if(!visible.length)box.append(node('p','list-empty',onlyAnalyzed?'当前没有已生成的建议':'今天暂无待处理个人消息'));
 }
 filter.onclick=()=>{onlyAnalyzed=!onlyAnalyzed;renderList();};
 async function refresh(force=false){
  if(!current()||pending&&!force)return;pending=true;const g=++generation;
  const button=document.getElementById('personal-inbox-refresh');if(button)button.disabled=true;
  try{
   const rows=await api().GetPersonalInbox();if(g!==generation||!current())return;
   items=(rows||[]).filter(x=>x.state==='new').sort((a,b)=>b.received-a.received);
   renderList();
   error().hidden=true;at=Date.now();
  }catch(e){if(g===generation&&current()){error().hidden=false;error().textContent='读取个人消息失败：'+text(e);}}
  finally{if(g===generation){pending=false;if(button)button.disabled=false;}}
 }
 document.getElementById('personal-inbox-refresh')?.addEventListener('click',()=>refresh(true));
 window.personalInbox={enter:()=>refresh(true),leave:()=>{generation++;pending=false;},poll:async()=>{if(Date.now()-at>5000)await refresh();}};
})();
