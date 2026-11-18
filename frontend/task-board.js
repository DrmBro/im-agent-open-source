/* 本机 IM 会话任务看板；仅查询既有任务账本，不重新执行任务。 */
'use strict';
(() => {
 const el=id=>document.getElementById(id);
 const names={all:'全部任务',active:'进行中',waiting:'等待处理',completed:'已完成',attention:'需要关注',closed:'已结束'};
 const hints={all:'全部保留记录',active:'排队、处理或发送中',waiting:'等待输入或本机处理',completed:'执行完成且回复已送达',attention:'失败或结果未确认',closed:'取消、过期或已合并'};
 const query={source:'',state:'',search:'',page:1,page_size:20};
 let generation=0,detailGeneration=0,documentGeneration=0,refreshAt=0,pending=false,view=null,selection='',signature='',scope='';
 const current=()=>page==='board';
 const stamp=value=>value?new Date(value*1000).toLocaleString():'未记录';
 const errorText=error=>String(error?.message||error);
 const connection=id=>(snapshot?.config?.connections||[]).find(c=>c.id===id)?.name||id||'未记录连接';
 function badge(state){return node('span','board-badge board-'+state,names[state]||'状态待确认');}
 function phase(item){if(item.stage?.startsWith('review_'))return {review_pending:'等待你审核，尚未回复联系人',review_approved:'已批准，等待发送',review_sending:'正在发送已批准的回复',review_sent:'已确认发送',review_manual:'你已标记自行回复，系统不再发送',review_ignored:'已忽略，未发送',review_unknown:'发送结果待核对，不自动重发'}[item.stage]||'审核状态待确认';if(item.execution==='completed')return item.delivery==='sent'?'执行完成，回复已确认送达':'执行完成，回复送达待核对';return taskStageNames[item.stage]||taskStatusNames[item.stage]||item.stage||'阶段未记录';}
 function clearDetail(){detailGeneration++;documentGeneration++;selection='';el('board-detail').removeAttribute('aria-busy');el('board-detail').replaceChildren(node('h3','','任务详情'),node('p','muted','选择一条任务，查看执行进度、送达证据和结果报告。'));}
 function leave(){generation++;pending=false;refreshAt=0;signature='';clearDetail();el('board-list').removeAttribute('aria-busy');el('board-refresh').disabled=false;}
 function renderStats(){
  const box=el('board-stats'),focused=document.activeElement?.dataset.boardState;box.replaceChildren();
  for(const [state,name] of Object.entries(names)){
   const button=node('button','board-stat board-'+state);button.type='button';button.dataset.boardState=state;const selected=(query.state||'all')===state;
   button.classList.toggle('selected',selected);button.setAttribute('aria-pressed',String(selected));button.append(node('span','board-stat-label',name),node('strong','',String(view?.counts?.[state]||0)),node('small','',hints[state]));
   button.onclick=()=>{query.state=state==='all'?'':state;query.page=1;refresh(true);};box.append(button);
  }
  if(focused!==undefined)Array.from(box.children).find(b=>b.dataset.boardState===focused)?.focus({preventScroll:true});
 }
 function renderList(){
  const list=el('board-list'),focused=document.activeElement?.dataset.boardTask;list.replaceChildren();
  for(const item of view.items||[]){
   const row=node('button','board-task');row.type='button';row.dataset.boardTask=item.id;row.classList.toggle('selected',selection===item.id);row.setAttribute('aria-pressed',String(selection===item.id));
   const heading=node('div','board-task-heading');heading.append(node('span','board-source','IM 会话请求'),badge(item.state));
   row.append(heading,node('strong','board-task-title',item.title||'会话请求'),node('span','board-task-meta',connection(item.connection)+' · 请求 '+item.id.slice(0,8)),node('span','board-task-phase',phase(item)));
   const times=node('span','board-task-times');times.append(node('span','','收到 '+stamp(item.received_at)),node('span','','更新 '+stamp(item.updated_at)));row.append(times);row.onclick=()=>openDetail(item);list.append(row);
  }
  if(!view.items?.length)list.append(node('p','board-empty',query.search||query.state?'没有符合条件的任务。':'暂无任务记录。'));
  el('board-list-title').textContent=names[query.state||'all'];el('board-total').textContent=`共 ${view.total} 条`;
  const pages=Math.max(1,Math.ceil(view.total/view.page_size));el('board-page').textContent=`第 ${view.page} / ${pages} 页`;el('board-prev').disabled=view.page<=1;el('board-next').disabled=view.page>=pages;
  if(focused!==undefined)Array.from(list.children).find(b=>b.dataset.boardTask===focused)?.focus({preventScroll:true});
 }
 async function refresh(force=false){
  if(!current()||pending&&!force)return;
  const nextScope=snapshot?.config?.state_dir||'';
  if(scope!==nextScope){leave();scope=nextScope;view=null;query.page=1;renderStats();el('board-list').replaceChildren(node('p','muted','正在读取任务…'));el('board-total').textContent='';}
  const request=++generation;pending=true;el('board-list').setAttribute('aria-busy','true');el('board-refresh').disabled=true;
  try{
   const result=await api().GetTaskBoard({...query});if(request!==generation||!current())return;
   view=result;query.page=result.page;el('board-error').hidden=true;el('board-sync-time').textContent='最近同步 '+stamp(result.captured_at);
   const warnings=[result.warning];if(status.phase!=='running'&&result.counts?.active)warnings.push('桥接当前未运行，进行中仅表示本机最后记录的状态。');el('board-warning').textContent=warnings.filter(Boolean).join('；');el('board-warning').hidden=!warnings.some(Boolean);
   const nextSignature=JSON.stringify([result.items,result.counts,result.total,result.page,result.page_size,query.state,snapshot?.revision,selection]);
   if(nextSignature!==signature){signature=nextSignature;renderStats();renderList();}
  }catch(error){if(request===generation&&current()){el('board-error').hidden=false;el('board-error').textContent='读取失败：'+errorText(error)+(view?'。当前保留上次内容，可能已过时。':'');}}
  finally{if(request===generation&&current()){pending=false;refreshAt=Date.now();el('board-list').removeAttribute('aria-busy');el('board-refresh').disabled=false;}}
 }
 async function openDetail(item){
  const request=++detailGeneration;documentGeneration++;selection=item.id;if(view)renderList();const box=el('board-detail');box.replaceChildren(node('h3','','任务详情'),node('p','muted','正在读取…'));box.setAttribute('aria-busy','true');
  try{
   const data=await api().GetTaskBoardDetail('session',item.id);if(request!==detailGeneration||!current())return;if(data.item?.id!==item.id)throw new Error('任务标识不匹配');
   const task=data.item;box.replaceChildren();const heading=node('div','board-detail-heading'),refreshButton=node('button','btn small','刷新详情'),close=node('button','btn small','关闭');refreshButton.type=close.type='button';refreshButton.onclick=()=>openDetail(task);close.onclick=()=>{clearDetail();if(view)renderList();};heading.append(node('h3','','任务详情'),refreshButton,close);box.append(heading,badge(task.state),node('h4','board-detail-task-title',task.title),node('p','board-task-phase',phase(task)),node('p','muted','详情读取于 '+new Date().toLocaleString()+'，可点击刷新详情更新。'));
   if(data.warning)box.append(node('p','notice warning',data.warning));if(task.detail)box.append(node('p','board-detail-note',task.detail));
   const metadata=node('dl','board-metadata');for(const [name,value] of [['连接',connection(task.connection)],['收到时间',stamp(task.received_at)],['更新时间',stamp(task.updated_at)],['任务编号',task.id],['会话编号',task.thread_id||'未记录']]){const row=node('div','');row.append(node('dt','',name),node('dd','',value));metadata.append(row);}box.append(metadata);
   const review=await api().GetPersonalReview(item.id);if(request!==detailGeneration||!current())return;
   if(review){renderPersonalReview(box,review,task);return;}
   renderRequestResult(box,data.result);
   if(task.thread_id&&task.execution==='completed'){const report=node('button','btn small','查看任务结果报告');report.type='button';report.onclick=()=>openReport(task);box.append(report);}
   const reader=node('section','board-reader');reader.id='board-reader';reader.hidden=true;box.append(reader);const manage=node('button','btn','打开任务与附件');manage.type='button';manage.onclick=()=>navigate('tasks');box.append(manage);
  }catch(error){if(request===detailGeneration&&current()){box.replaceChildren(node('h3','','任务详情'),node('p','detect-error',errorText(error)));const retry=node('button','btn','重试详情');retry.type='button';retry.onclick=()=>openDetail(item);box.append(retry);}}
  finally{if(request===detailGeneration)box.removeAttribute('aria-busy');}
 }
 function renderPersonalReview(box,r,item){
  const labels={ready:'可以处理',needs_info:'需要补充信息',manual:'需要本人处理'};
  box.append(node('h4','','处理判断：'+(labels[r.assessment]||'需要本人核对')),node('p','',r.reason),node('p','muted','发送人：'+(r.peer_name||r.peer)+' · '+r.peer));
  const groupTarget=r.target?.includes(':group:');const targetLabel=groupTarget?'原群会话 '+r.target.split(':group:')[1]:'原私聊 '+(r.peer_name||r.peer);box.append(node('p','notice','批准后回复到：'+targetLabel));
  const original=node('pre');original.style.whiteSpace='pre-wrap';original.style.overflowWrap='anywhere';original.textContent=r.input||'原文已清理';box.append(node('h4','','收到的消息'),original);
  const field=node('textarea','board-document');field.rows=9;field.maxLength=12000;field.value=r.approved_text||r.draft;field.readOnly=r.state!=='pending';field.setAttribute('aria-label','待审核回复草稿');
  box.append(node('h4','',r.state==='pending'?'回复草稿（可修改，尚未发送）':'审核记录中的回复'),field);
  const feedback=node('p','muted');box.append(feedback);
  const copy=node('button','btn small','复制草稿');copy.type='button';copy.onclick=async()=>{try{await navigator.clipboard.writeText(field.value);feedback.textContent='已复制；请自行回复后，再标记“我已自行回复”。';}catch{field.focus();field.select();feedback.textContent='已选中草稿，请使用系统复制快捷键。';}};box.append(copy);
  if(r.state!=='pending'){box.append(node('p','muted',r.state==='unknown'?'发送结果不确定，请先到钉钉核对；不要重复发送。':'这条审核记录已处理，不能再次发送。'));return;}
  const action=(label,kind)=>{
   const button=node('button',kind==='send'?'btn primary':'btn small',label);button.type='button';
   button.onclick=()=>task(async()=>{
    const text=field.value;
    if(kind==='send'&&!text.trim())throw new Error('请填写回复正文');
    const descriptions={send:`将以你的钉钉个人身份回复到 ${targetLabel}，保留 AI 标记。\n\n${text.slice(0,600)}${text.length>600?'…':''}`,manual:'仅记录你已自行处理，IM Agent 不会发送这条回复。',ignored:'保留审核记录，不向联系人发送。'};
    if(kind!=='save'&&!await confirmAction(label+'？',descriptions[kind]))return;
    try{await api().ReviewPersonalReply({event_id:r.event_id,version:r.version,action:kind,text},true);}catch(error){feedback.textContent='操作失败：'+errorText(error);feedback.className='notice warning';throw error;}
    note(kind==='send'?'已批准，后台将按投递账本发送；请核对最终状态。':'审核记录已更新。');await refresh(true);await openDetail(item);
   });box.append(button);
  };
  action('保存草稿','save');if(r.can_send)action('确认并发送','send');else box.append(node('p','notice warning','分析没有完成，请自行回复；此记录不会由系统发送。'));action('我已自行回复','manual');action('忽略','ignored');
 }
 async function openReport(item){
  const request=++documentGeneration,id=selection,reader=el('board-reader');if(!reader)return;reader.hidden=false;reader.replaceChildren(node('p','muted','正在读取本机已封存报告…'));
  const valid=()=>request===documentGeneration&&id===selection&&current();
  try{
   const data=await api().GetTaskDelivery(item.thread_id,item.id);if(!valid())return;if(data.event_id!==item.id||data.thread_id!==item.thread_id||typeof data.report!=='string')throw new Error('报告标识不匹配');
   const body=node('textarea','board-document'),copy=node('button','btn small','复制正文'),close=node('button','btn small','关闭正文'),message=node('p','muted','已读取本机报告。');body.readOnly=true;body.rows=16;body.value=data.report;body.setAttribute('aria-label','任务报告正文');copy.type=close.type='button';
   copy.onclick=async()=>{try{if(!navigator.clipboard?.writeText)throw new Error('剪贴板不可用');await navigator.clipboard.writeText(body.value);if(valid())message.textContent='已复制。';}catch{if(valid()){body.focus();body.select();message.textContent='已选中正文，请使用系统复制快捷键。';}}};close.onclick=()=>{documentGeneration++;reader.hidden=true;reader.replaceChildren();};reader.replaceChildren(node('h4','','任务结果报告'),copy,close,message,body);
  }catch(error){if(valid())reader.replaceChildren(node('p','detect-error','报告读取失败：'+errorText(error)));}
 }

 function applyFilters(){query.source=el('board-source').value;query.search=el('board-search').value.trim();query.page=1;refresh(true);}
 el('board-filters').onsubmit=e=>{e.preventDefault();applyFilters();};el('board-source').onchange=applyFilters;
 el('board-reset').onclick=()=>{query.source='';query.state='';query.search='';query.page=1;el('board-search').value='';el('board-source').value='';refresh(true);};el('board-size').onchange=()=>{query.page_size=Number(el('board-size').value);query.page=1;refresh(true);};el('board-prev').onclick=()=>{query.page=Math.max(1,query.page-1);refresh(true);};el('board-next').onclick=()=>{query.page++;refresh(true);};el('board-refresh').onclick=()=>refresh(true);
 window.taskBoard={enter:()=>{signature='';refresh(true);},leave,poll:async()=>{if(Date.now()-refreshAt>5000)await refresh();}};
})();
