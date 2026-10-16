// 当前安装版的只读请求目录与原生审批入口。复用现有连接，不注册第二个 AppView。
async function desktopApprovals(input) {
  if (!input || !["snapshot","respond"].includes(input.action)) return {state:"unavailable"};
  let manager, subscription, invoked=false;
  const dispose=()=>{try{subscription?.unsubscribe?.().catch?.(()=>{});}catch{};try{manager?.[Symbol.dispose]?.();}catch{}};
  try {
    const shared=await import('/assets/app-shared-01b9f268c4a3.js');
    if(shared.Kat?.appServerManagers){const opened=await shared.Kat.appServerManagers.open('local');if(opened.status==='ready')manager=opened.manager;}
    if(!manager){
      const initial=await import('/assets/app-initial-3ccf4886957f.js');
      let root;for(const el of document.querySelectorAll('#root,#app')){const key=Object.keys(el).find(k=>k.startsWith('__reactContainer$'));if(key){root=el[key]?.stateNode?.current??el[key];break;}}
      if(!root || typeof initial.YPr!=='function') return {state:"unavailable"};
      const stack=[root],seen=new Set(),scopes=new Map();
      while(stack.length && seen.size<12000){const node=stack.pop();if(!node||seen.has(node))continue;seen.add(node);if(node.child)stack.push(node.child);if(node.sibling)stack.push(node.sibling);let hook=node.memoizedState;
        for(let n=0;hook&&n<80;n++,hook=hook.next){const value=hook.memoizedState?.current;if(value?.scope===shared.Cot&&typeof value.get==='function'&&value.node)scopes.set(value.node,value);}}
      if(stack.length || scopes.size!==1)return {state:"unavailable"};
      manager=initial.YPr([...scopes.values()][0],'local').dup();
    }
    const hash=async value=>[...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(JSON.stringify(value))))].map(n=>n.toString(16).padStart(2,'0')).join('');
    const requests=async thread=>{const view=manager.getConversation(thread);if(await view.id!==thread)throw Error('identity');const rows=await view.requests;if(!Array.isArray(rows))throw Error('coverage');return rows;};
    const supported=r=>r?.method==='item/commandExecution/requestApproval' && (r.params?.kind??'command')==='command' && typeof r.params.threadId==='string' && typeof r.params.turnId==='string' && typeof r.params.itemId==='string' &&
      ((typeof r.id==='string'&&r.id.length>0&&r.id.length<=256)||(typeof r.id==='number'&&Number.isSafeInteger(r.id)));
    if(input.action==='snapshot'){
      if(!Array.isArray(input.threads)||input.threads.length>512)return {state:"unavailable"};
      const allowed=new Set(input.threads),active=await manager.getWorkInProgress();if(!Array.isArray(active))return {state:"unavailable"};
      const targets=[...new Set(active.filter(v=>v.kind==='pending-request'&&allowed.has(v.conversationId)).map(v=>v.conversationId))];
      const out=[];for(const thread of targets){for(const r of await requests(thread)){if(!supported(r)||r.params.threadId!==thread)continue;if(out.length>=64)return {state:"snapshot",complete:false,requests:out};
        const command=typeof r.params.command==='string'?r.params.command:'';
        out.push({requestId:r.id,threadId:thread,turnId:r.params.turnId,itemId:r.params.itemId,fingerprint:await hash(r),command:command.slice(0,1800),reason:String(r.params.reason??'').slice(0,500),allowOnce:command.length>0&&command.length<=1800});}}
      return {state:"snapshot",complete:true,requests:out};
    }
    if(!['accept','decline'].includes(input.decision)||typeof input.threadId!=='string')return {state:"unavailable"};
    const matches=(await requests(input.threadId)).filter(r=>supported(r)&&JSON.stringify(r.id)===JSON.stringify(input.requestId)&&r.params.threadId===input.threadId&&r.params.turnId===input.turnId&&r.params.itemId===input.itemId);
    if(matches.length!==1 || await hash(matches[0])!==input.fingerprint)return {state:"stale"};
    if(input.decision==='accept'&&(!matches[0].params.command||matches[0].params.command.length>1800))return {state:"stale"};
    let confirm;const resolved=new Promise(resolve=>{confirm=resolve});
    subscription=await manager.subscribe({type:'notification',key:{hostId:'local'},methods:['serverRequest/resolved'],listener:n=>{
      if(n?.method==='serverRequest/resolved'&&n.params?.threadId===input.threadId&&JSON.stringify(n.params.requestId)===JSON.stringify(input.requestId))confirm(true);
    }});
    // 订阅后再次检查；不使用过期请求，不改写策略或会话级许可。
    const live=(await requests(input.threadId)).filter(r=>supported(r)&&JSON.stringify(r.id)===JSON.stringify(input.requestId));
    if(live.length!==1||await hash(live[0])!==input.fingerprint)return {state:"stale"};
    invoked=true;
    await manager.replyWithCommandExecutionApprovalDecision(input.threadId,input.requestId,input.decision);
    let timer;const confirmed=await Promise.race([resolved,new Promise(resolve=>{timer=setTimeout(()=>resolve(false),3000)})]);clearTimeout(timer);
    return {state:confirmed?'resolved':'sent'};
  }catch{return {state:invoked?'uncertain':'unavailable'};}finally{dispose();}
}
