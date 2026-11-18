// 已核对的桌面协议能力集中登记；未知版本显式失败，不猜测可执行入口。
async function desktopManagerAdapter() {
 const profiles=[
  {id:'chatgpt-20261028',shared:'/assets/app-shared-e9f258fd3588.js',initial:'/assets/app-initial-bc8256c3cccb.js',registry:'Sst',scope:'uct',factory:'hor',questions:{lwr:'jJn',vSr:'NKn',_9n:'BSn',g9n:'zSn',S9n:'GSn',d9n:'NSn',otr:'qwn',Jwt:'xU',Gwt:'gU',sEr:'PJn'}},
  {id:'chatgpt-26.1021',shared:'/assets/app-shared-2cae01273bae.js',initial:'/assets/app-initial-feb30b196d11.js',registry:'Qot',scope:'Bst',factory:'irr',questions:{lwr:'aqn',vSr:'sGn',_9n:'axn',g9n:'ixn',S9n:'uxn',d9n:'Qbn',otr:'yCn',Jwt:'UV',Gwt:'RV',sEr:'cqn'}},
  {id:'codex-async-20261022',shared:'/assets/app-shared-60f4bb85435f.js',initial:'/assets/app-initial-f3e281fb9a71.js',registry:'ist',scope:'Qst',factory:'FBr',questions:{lwr:'lwr',vSr:'vSr',_9n:'_9n',g9n:'g9n',S9n:'S9n',d9n:'d9n',otr:'otr',Jwt:'Jwt',Gwt:'Gwt',sEr:'sEr',b9n:'b9n'}},
  {id:'codex-legacy',shared:'/assets/app-shared-01b9f268c4a3.js',initial:'/assets/app-initial-3ccf4886957f.js',registry:'Kat',scope:'Cot',factory:'YPr'}
 ];
 let manager,shared,profile,scope,initial;
 const dispose=()=>{try{manager?.[Symbol.dispose]?.();}catch{}};
 try {
  for(const item of profiles){try{shared=await import(item.shared);profile=item;break;}catch{}}
  if(!profile)return {state:'unavailable',reason:'unsupported_desktop_version'};
  const registry=shared[profile.registry]?.appServerManagers;
  if(registry){const opened=await registry.open('local');if(opened.status==='ready')manager=opened.manager;}
  if(!manager||profile.questions){
   initial=await import(profile.initial);
   let root;for(const el of document.querySelectorAll('#root,#app')){const key=Object.keys(el).find(k=>k.startsWith('__reactContainer$'));if(key){root=el[key]?.stateNode?.current??el[key];break;}}
   if(root&&shared[profile.scope]!=null){
    const stack=[root],seen=new Set(),scopes=new Map();
    while(stack.length&&seen.size<12000){const node=stack.pop();if(!node||seen.has(node))continue;seen.add(node);if(node.child)stack.push(node.child);if(node.sibling)stack.push(node.sibling);
     let hook=node.memoizedState;for(let n=0;hook&&n<80;n++,hook=hook.next){const value=hook.memoizedState?.current;if(value?.scope===shared[profile.scope]&&typeof value.get==='function'&&value.node)scopes.set(value.node,value);}}
    if(!stack.length&&scopes.size===1)scope=[...scopes.values()][0];
   }
   if(!manager){
    if(!scope||typeof initial[profile.factory]!=='function')return {state:'unavailable',reason:'desktop_scope_unavailable',adapter:profile.id};
    manager=initial[profile.factory](scope,'local').dup();
   }
  }
  if(typeof manager.getWorkInProgress!=='function'||typeof manager.getConversation!=='function'){dispose();return {state:'unavailable',reason:'request_catalog_unavailable',adapter:profile.id};}
  let questionAPI=null;
  if(profile.questions&&scope&&initial){
   const candidate=Object.fromEntries(Object.entries(profile.questions).map(([name,exportName])=>[name,initial[exportName]]));
   if(['lwr','vSr','S9n','d9n','Jwt','Gwt','sEr'].every(k=>typeof candidate[k]==='function')&&['_9n','g9n','otr'].every(k=>candidate[k]!=null))questionAPI=candidate;
  }
  const capabilities=['command'];
  if(typeof manager.replyWithUserInputResponse==='function')capabilities.push('input');
  if(questionAPI)capabilities.push('async_input');
  return {state:'ready',adapter:profile.id,manager,scope,initial:questionAPI,capabilities,dispose};
 }catch{dispose();return {state:'unavailable',reason:'desktop_adapter_unavailable',adapter:profile?.id};}
}
