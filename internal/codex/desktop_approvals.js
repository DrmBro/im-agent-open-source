// 当前安装版的只读请求目录与原生审批入口。复用现有连接，不注册第二个 AppView。
async function desktopApprovals(input, asyncQuestions, openAdapter) {
  if (!input || !["snapshot","respond"].includes(input.action)) return {state:"unavailable"};
  let manager, subscription, connection, invoked=false;
  const dispose=()=>{try{subscription?.unsubscribe?.().catch?.(()=>{});}catch{};try{connection?.dispose?.();}catch{}};
  try {
    if(typeof openAdapter!=='function')return {state:'unavailable',reason:'adapter_missing'};
    connection=await openAdapter();
    if(connection.state!=='ready')return connection;
    manager=connection.manager;
    const {initial,scope}=connection;
    const metadata={adapter:connection.adapter,capabilities:connection.capabilities};
    const hash=async value=>[...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(JSON.stringify(value))))].map(n=>n.toString(16).padStart(2,'0')).join('');
    const requests=async thread=>{const view=manager.getConversation(thread);if(await view.id!==thread)throw Error('identity');const rows=await view.requests;if(!Array.isArray(rows))throw Error('coverage');return rows;};
    const isInput=r=>r?.method==='item/tool/requestUserInput';
    const validQuestions=questions=>{
      if(!Array.isArray(questions)||questions.length<1||questions.length>10)return false;
      const ids=new Set();
      return questions.every(q=>{
        if(!q||typeof q.id!=='string'||!q.id||q.id.length>256||ids.has(q.id)||typeof q.question!=='string'||!q.question.trim()||q.isSecret===true)return false;
        ids.add(q.id);
        return (q.header==null||typeof q.header==='string')&&(q.options==null||(Array.isArray(q.options)&&q.options.length<=100&&q.options.every(o=>o&&typeof o.label==='string'&&o.label.trim()&&(o.description==null||typeof o.description==='string'))));
      })&&JSON.stringify(questions).length<=6000;
    };
    const supported=r=>((r?.method==='item/commandExecution/requestApproval' && (r.params?.kind??'command')==='command') || (isInput(r)&&validQuestions(r.params?.questions))) && typeof r.params.threadId==='string' && typeof r.params.turnId==='string' && typeof r.params.itemId==='string' &&
      ((typeof r.id==='string'&&r.id.length>0&&r.id.length<=256)||(typeof r.id==='number'&&Number.isSafeInteger(r.id)));
    const asyncCall=payload=>asyncQuestions(payload,{initial,scope,manager,hash,validQuestions});
    if(input.action==='snapshot'){
      if(!Array.isArray(input.threads)||input.threads.length>512)return {state:"unavailable"};
      const allowed=new Set(input.threads),active=await manager.getWorkInProgress();if(!Array.isArray(active))return {state:"unavailable"};
      const targets=[...new Set(active.filter(v=>v.kind==='pending-request'&&allowed.has(v.conversationId)).map(v=>v.conversationId))];
      const out=[];let commandComplete=true;for(const thread of targets){for(const r of await requests(thread)){if(!supported(r)||r.params.threadId!==thread){commandComplete=false;continue;}if(out.length>=64)return {...metadata,state:'snapshot',complete:false,coveredKinds:connection.capabilities,requests:out};
        const command=typeof r.params.command==='string'?r.params.command:'';
        out.push({kind:isInput(r)?'input':'command',requestId:r.id,threadId:thread,turnId:r.params.turnId,itemId:r.params.itemId,fingerprint:await hash(r),command:command.slice(0,1800),reason:String(r.params.reason??'').slice(0,500),allowOnce:command.length>0&&command.length<=1800,questions:isInput(r)?r.params.questions:undefined});}}
      if(connection.capabilities.includes('async_input')){
        if(typeof asyncQuestions!=='function')return {state:"unavailable"};
        const result=await asyncCall(input);
        if(result.state!=='snapshot')return {...metadata,state:'snapshot',complete:false,coveredKinds:['command','input'],requests:out,reason:'async_input_unavailable'};
        for(const request of result.requests){if(out.length>=64)return {...metadata,state:'snapshot',complete:false,coveredKinds:connection.capabilities,requests:out};out.push(request);}
        return {...metadata,state:'snapshot',complete:commandComplete&&result.complete,coveredKinds:connection.capabilities,requests:out};
      }
      return {...metadata,state:'snapshot',complete:commandComplete,coveredKinds:connection.capabilities,requests:out};
    }
    if(typeof input.threadId!=='string')return {state:"unavailable"};
    if(input.kind==='async_input'){
      if(!connection.capabilities.includes('async_input')||typeof asyncQuestions!=='function')return {state:"unavailable"};
      // 异步提问通过原生消息提交器回传；从这里开始结果未知时不重试。
      invoked=true;
      return await asyncCall(input);
    }
    const matches=(await requests(input.threadId)).filter(r=>supported(r)&&JSON.stringify(r.id)===JSON.stringify(input.requestId)&&r.params.threadId===input.threadId&&r.params.turnId===input.turnId&&r.params.itemId===input.itemId);
    if(matches.length!==1 || await hash(matches[0])!==input.fingerprint)return {state:"stale"};
    const request=matches[0];
    let response;
    if(isInput(request)){
      if(typeof manager.replyWithUserInputResponse!=='function'||!input.answers||typeof input.answers!=='object'||Array.isArray(input.answers))return {state:"unavailable"};
      const questions=request.params.questions,keys=Object.keys(input.answers),answers=Object.create(null);
      // 空答案与原生 Skip 一致；非空提交必须完整回答所有问题。
      if(keys.length!==0&&keys.length!==questions.length)return {state:"unavailable"};
      for(const key of keys){
        const q=questions.find(q=>q.id===key),a=input.answers[key];
        if(!q||!a||typeof a!=='object')return {state:"unavailable"};
        let text;
        if(typeof a.text==='string'&&a.text.trim()&&a.text.length<=4000)text=a.text.trim();
        else if(Number.isSafeInteger(a.option)&&a.option>=0&&a.option<(q.options?.length??0))text=q.options[a.option].label;
        else return {state:"unavailable"};
        answers[key]={answers:[text]};
      }
      response={answers};
    }else{
      if(!['accept','decline'].includes(input.decision))return {state:"unavailable"};
      if(input.decision==='accept'&&(!request.params.command||request.params.command.length>1800))return {state:"stale"};
    }
    let confirm;const resolved=new Promise(resolve=>{confirm=resolve});
    subscription=await manager.subscribe({type:'notification',key:{hostId:'local'},methods:['serverRequest/resolved'],listener:n=>{
      if(n?.method==='serverRequest/resolved'&&n.params?.threadId===input.threadId&&JSON.stringify(n.params.requestId)===JSON.stringify(input.requestId))confirm(true);
    }});
    // 订阅后再次检查；不使用过期请求，不改写策略或会话级许可。
    const live=(await requests(input.threadId)).filter(r=>supported(r)&&JSON.stringify(r.id)===JSON.stringify(input.requestId));
    if(live.length!==1||await hash(live[0])!==input.fingerprint)return {state:"stale"};
    invoked=true;
    if(isInput(request))await manager.replyWithUserInputResponse(input.threadId,input.requestId,response);
    else await manager.replyWithCommandExecutionApprovalDecision(input.threadId,input.requestId,input.decision);
    let timer;const confirmed=await Promise.race([resolved,new Promise(resolve=>{timer=setTimeout(()=>resolve(false),3000)})]);clearTimeout(timer);
    return {state:confirmed?'resolved':'sent'};
  }catch{return {state:invoked?'uncertain':'unavailable'};}finally{dispose();}
}
