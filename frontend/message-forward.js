/* 手动投递公开消息；不改变会话绑定，不触发模型执行。 */
'use strict';
(() => {
 const root=document.getElementById('message-forward');
 if(!root)return;
 const body=node('div'),load=node('button','btn','加载会话与群缓存');load.type='button';root.append(load,body);
 let threads=[],categories=[],messages=[],before='',cursorStack=[],loading=false,generation=0,built=false;
 let selectedGroup=null,directoryGeneration=0,searchGeneration=0,searchTimer;
 const group=node('details','forward-group-picker'),groupLabel=node('summary','','请选择目标群'),groupSearch=node('input'),groupTree=node('div','forward-group-tree');
 groupSearch.type='search';groupSearch.placeholder='搜索全部群名称';group.append(groupLabel,node('p','muted','读取本机群缓存；后台每 5 分钟更新目录，每分钟更新最近消息时间。'),groupSearch,groupTree);
 const info=node('p','muted'),search=node('input'),source=node('select'),message=node('select'),preview=node('textarea','board-document'),connection=node('select'),older=node('button','btn small','更早消息'),newer=node('button','btn small','返回较新消息'),send=node('button','btn primary','预览并投递'),records=node('div');
 search.type='search';search.placeholder='搜索 Codex 会话名称或 ID';preview.rows=12;preview.maxLength=24000;older.type=newer.type=send.type='button';
 const label=(text,control)=>{const wrap=node(control===group?'div':'label','field');wrap.append(node('span','',text),control);return wrap;};
 body.append(info);
 const call=r=>api().MessageForward(r);
 const error=e=>String(e?.message||e);
 const option=(value,text)=>{const o=node('option','',text);o.value=value;return o;};
 const stateNames={failed:'钉钉已拒绝，消息未发送',sent:'已送达',sending:'发送中或待核对',unknown:'送达未确认，请核对群消息'};
 function showRecords(rows){records.replaceChildren(node('h4','','最近投递记录'));for(const r of rows||[]){records.append(node('p','muted',`${new Date(r.updated*1000).toLocaleString()} · ${r.target} · ${stateNames[r.state]||r.state} · 已确认 ${r.sent}/${r.total} 段`));}if(!rows?.length)records.append(node('p','muted','暂无投递记录'));}
 async function run(fn){if(loading)return;loading=true;for(const c of [search,source,message,preview,connection,groupSearch])c.disabled=true;load.disabled=send.disabled=older.disabled=newer.disabled=true;try{await fn();}catch(e){info.textContent='操作失败：'+error(e);info.className='notice warning';}finally{loading=false;for(const c of [search,source,message,preview,connection,groupSearch])c.disabled=false;load.disabled=false;send.disabled=!messages.some(m=>m.id===message.value);older.disabled=!body.dataset.more;newer.disabled=!cursorStack.length;}}
 function renderSources(){const previous=source.value;source.replaceChildren(option('','请选择 Codex 会话'));const q=search.value.trim().toLowerCase();for(const t of threads){if(!q||(t.name+' '+t.id).toLowerCase().includes(q))source.append(option(t.id,t.name+' · '+t.id.slice(-8)));}if([...source.options].some(o=>o.value===previous))source.value=previous;else resetMessages();}
 function resetMessages(){generation++;messages=[];before='';cursorStack=[];message.replaceChildren(option('','请选择消息'));preview.value='';body.dataset.more='';send.disabled=older.disabled=newer.disabled=true;}
 function selectMessage(){const m=messages.find(m=>m.id===message.value);preview.value=m?.text||'';send.disabled=!m;info.className='notice';info.textContent=m?.truncated?'此消息超过正文上限，预览已截取，请编辑后确认。':'投递正文时会上传其中的图片并内嵌显示，普通文件不上传。不改变原会话绑定或自动同步。';}
 async function loadMessages(){const id=source.value;if(!id){resetMessages();return;}const request=++generation;const result=await call({action:'messages',thread:id,before});if(request!==generation||source.value!==id)return;messages=result.messages||[];body.dataset.more=result.more?'yes':'';message.replaceChildren(option('','请选择消息'));for(const m of [...messages].reverse()){message.append(option(m.id,(m.role==='assistant'?'助手回复':'用户消息')+' · '+m.text.replace(/\s+/g,' ').slice(0,100)));}preview.value='';info.textContent=messages.length?'请选择要投递的一条消息。':'这一页没有可投递的公开消息。';}
 function chooseGroup(g){if(loading)return;selectedGroup=g;groupLabel.textContent=g.name;group.open=false;}
 function groupRows(parent,rows){
  if(!rows.length){parent.append(node('p','muted','此分组没有可投递的群聊。'));return;}
  const pending=rows.filter(g=>g.time_state==='loading').length,unknown=rows.filter(g=>g.time_state==='unavailable').length;
  parent.append(node('p','muted',pending?`按最近消息倒序，${pending}个群的时间读取中，排序会自动更新。`:unknown?`已按最近消息倒序；${unknown}个群暂无法读取时间，放在末尾。`:'按最近消息时间倒序'));
  for(const g of rows){const when=g.last_message_at?new Date(g.last_message_at).toLocaleString():g.time_state==='loading'?'时间读取中':g.time_state==='empty'?'暂无可读取消息':'时间暂不可读';const button=node('button','btn forward-group-option',(g.pinned?'📌 ':'')+g.name+(g.members?' · '+g.members+'人':'')+' · '+when+' · '+g.id.slice(3,11));button.type='button';button.title=g.id;button.onclick=()=>chooseGroup(g);parent.append(button);}
 }
 function renderGroups(){
  const version=++directoryGeneration;searchGeneration++;selectedGroup=null;groupLabel.textContent='请选择目标群';groupSearch.value='';groupTree.replaceChildren();
  const account=connection.value;
  for(const cat of categories.filter(c=>c.connection===account)){
   const branch=node('details'),title=node('summary','',cat.name),list=node('div','forward-group-children');branch.append(title,list);groupTree.append(branch);let inflight=false,shown=false,timer;
   const refreshBranch=async()=>{if(!branch.open||inflight||version!==directoryGeneration)return;inflight=true;if(!shown)list.replaceChildren(node('p','muted','正在读取群列表…'));
    try{const data=await call({action:'category_groups',connection:account,category:cat.id});if(version!==directoryGeneration)return;list.replaceChildren();const rows=data.groups||[];groupRows(list,rows);shown=true;if(rows.some(g=>g.time_state==='loading'))timer=setTimeout(()=>{if(page==='board'&&group.open&&branch.open)refreshBranch();},3000);}
    catch(e){if(version===directoryGeneration){list.replaceChildren(node('p','notice warning','读取失败：'+error(e)+'；收起后重新展开可重试。'));}}
    finally{inflight=false;}
   };
   branch.ontoggle=()=>{clearTimeout(timer);if(branch.open)refreshBranch();};
  }
  if(!groupTree.children.length)groupTree.append(node('p','muted','没有可读取的分组，请重新加载。'));
 }
 groupSearch.oninput=()=>{
  clearTimeout(searchTimer);const q=groupSearch.value.trim().toLowerCase(),request=++searchGeneration,version=directoryGeneration,account=connection.value;
  if(!q){const chosen=selectedGroup;renderGroups();if(chosen){selectedGroup=chosen;groupLabel.textContent=chosen.name;}return;}
  const refreshSearch=async()=>{if(request!==searchGeneration||version!==directoryGeneration)return;try{const data=await call({action:'category_groups',connection:account,category:'all'});if(request!==searchGeneration||version!==directoryGeneration)return;groupTree.replaceChildren(node('p','muted','搜索结果（全部群）'));const rows=(data.groups||[]).filter(g=>(g.name+' '+g.id).toLowerCase().includes(q));groupRows(groupTree,rows);if(rows.some(g=>g.time_state==='loading'))searchTimer=setTimeout(()=>{if(page==='board'&&group.open)refreshSearch();},3000);}catch(e){if(request===searchGeneration&&version===directoryGeneration)groupTree.replaceChildren(node('p','notice warning','搜索失败：'+error(e)));}};
  groupTree.replaceChildren(node('p','muted','正在搜索全部群…'));searchTimer=setTimeout(refreshSearch,300);
 };
 load.onclick=()=>run(async()=>{const [result,directory]=await Promise.all([call({action:'catalog'}),call({action:'categories'})]);threads=result.threads||[];categories=directory.categories||[];resetMessages();renderSources();connection.replaceChildren();for(const c of result.connections||[])connection.append(option(c.id,c.name));renderGroups();showRecords(result.records);if(!built){built=true;body.append(label('搜索会话',search),label('来源会话',source),label('指定消息（每页最多30条）',message),newer,older,label('投递正文（可编辑）',preview),label('钉钉账号',connection),label('目标群（按钉钉分组选择）',group),send,records);}info.textContent=connection.options.length?'请选择来源会话。':'请先配置钉钉个人账号并开启群 @接入。';});
 search.oninput=()=>{if(!loading)renderSources();};source.onchange=()=>{resetMessages();run(loadMessages);};message.onchange=selectMessage;connection.onchange=renderGroups;
 older.onclick=()=>run(async()=>{if(!messages.length)return;cursorStack.push(before);before=messages[0].id;await loadMessages();});newer.onclick=()=>run(async()=>{before=cursorStack.pop()||'';await loadMessages();});
 send.onclick=()=>run(async()=>{
  const m=messages.find(m=>m.id===message.value);if(!m)throw new Error('请选择消息');
  const target=selectedGroup?.id;const text=preview.value;
  if(!connection.value||!target||!text.trim())throw new Error('请选择钉钉账号、目标群并填写正文');
  const request={action:'send',thread:source.value,before,message:m.id,fingerprint:m.fingerprint,connection:connection.value,group:target,text,confirmed:true};
  await call({...request,action:'preview',confirmed:false});
  if(!await confirmAction('确认投递到钉钉群？',`账号：${request.connection}\n目标群：${selectedGroup.name}\n群 ID：${target}\n来源会话：${request.thread}\n消息：${m.id}\n\n${text}\n\n将以你的钉钉个人身份发送，保留 AI 标记；正文中的图片会上传，普通文件不上传。`))return;
  try{const result=await call(request);info.className='notice';info.textContent=(stateNames[result.state]||result.state)+'。相同来源、目标和正文重复点击不会再次发送。';}finally{try{showRecords((await call({action:'records'})).records);}catch{}}
 });
})();
