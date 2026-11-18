// 新版异步 Question 保存在公开助手消息中，不属于 conversation.requests。
// 读取原生问题状态；回答复用桌面原生提交器，保留输入框草稿与附件。
async function desktopAsyncQuestions(input, env) {
  const {initial,scope,manager,hash,validQuestions}=env;
  const unavailable=()=>({state:'unavailable'});
  if(!initial||!scope||typeof initial.lwr!=='function'||typeof initial.vSr!=='function')return unavailable();
  const read=threadId=>{
    const selected=scope.get(initial._9n,{hostId:'local',threadId});
    if(selected==null)return null;
    const key=selected.selectedQuestionKey;
    if(key?.hostId!=='local'||key.threadId!==threadId||typeof key.entityKey!=='string'||!Array.isArray(selected.questionIds))throw Error('question_identity');
    const turn=initial.vSr(scope,key);
    if(!turn||typeof turn.turnId!=='string'||!Array.isArray(turn.items))throw Error('question_coverage');
    const source=initial.lwr(turn.items),questions=[];
    for(const id of selected.questionIds){
      const matches=source.filter(q=>q.id===id),state=scope.get(initial.g9n,{...key,itemId:id});
      if(matches.length!==1||state?.turnId!==turn.turnId)throw Error('question_identity');
      if(state.isSkipped||state.lastSubmission!=null&&state.draft.trim()===state.lastSubmission.trim())continue;
      const q=matches[0];
      if(typeof q.title!=='string'||!Array.isArray(q.options)||q.options.some(o=>typeof o!=='string'))throw Error('question_format');
      questions.push({id:q.id,header:'',question:q.title,isSecret:false,options:q.options.map(label=>({label,description:''}))});
    }
    if(!questions.length)return null;
    if(!validQuestions(questions))throw Error('question_bounds');
    return {kind:'async_input',requestId:'async-question:'+key.entityKey,threadId,turnId:turn.turnId,itemId:key.entityKey,entityKey:key.entityKey,questions};
  };
  if(input.action==='snapshot'){
    const requests=[];let complete=true;
    for(const threadId of input.threads){
      try{const request=read(threadId);if(request)requests.push({...request,fingerprint:await hash(request)});}
      catch{complete=false;}
    }
    return {state:'snapshot',complete,requests};
  }
  if(input.action!=='respond'||input.kind!=='async_input')return unavailable();
  let request;
  try{request=read(input.threadId);}catch{return unavailable();}
  if(!request||request.turnId!==input.turnId||request.itemId!==input.itemId||request.entityKey!==input.entityKey||request.requestId!==input.requestId||await hash(request)!==input.fingerprint)return {state:'stale'};
  const original=JSON.stringify(request),key={hostId:'local',threadId:request.threadId,entityKey:request.entityKey};
  const current=()=>{try{return JSON.stringify(read(request.threadId))===original;}catch{return false;}};
  const states=request.questions.map(q=>scope.get(initial.g9n,{...key,itemId:q.id}));
  if(states.some(s=>!s||s.isSubmitting))return {state:'stale'};
  if(!input.answers||typeof input.answers!=='object'||Array.isArray(input.answers))return unavailable();
  const ids=Object.keys(input.answers);
  if(ids.length===0){
    // 原生异步 Skip 只标记跳过并关闭问题，不伪造用户消息或审批决定。
    if(typeof initial.S9n!=='function'||typeof initial.d9n!=='function')return unavailable();
    if(!current())return {state:'stale'};
    const selected=scope.get(initial._9n,{hostId:'local',threadId:request.threadId});
    for(const q of request.questions)initial.S9n(scope,{...key,itemId:q.id});
    initial.d9n(scope,selected.selectedQuestionKey);
    return {state:'resolved'};
  }
  if(ids.length!==request.questions.length||ids.some(id=>!request.questions.some(q=>q.id===id)))return unavailable();
  const replies=[];
  for(const q of request.questions){
    const answer=input.answers[q.id];let text;
    if(typeof answer?.text==='string'&&answer.text.trim()&&answer.text.length<=4000)text=answer.text.trim();
    else if(Number.isSafeInteger(answer?.option)&&answer.option>=0&&answer.option<q.options.length)text=q.options[answer.option].label;
    else return unavailable();
    replies.push({questionItemId:q.id,question:q.question,answer:text});
  }
  const meta=scope.get(initial.otr,{hostId:'local',threadId:request.threadId});
  if(typeof meta?.cwd!=='string'||!meta.cwd||typeof initial.Jwt!=='function'||typeof initial.Gwt!=='function'||typeof initial.sEr!=='function')return unavailable();
  let follow;
  try{
    follow=await manager.getFollowUpContext(request.threadId);
    if(!follow?.latestCollaborationMode||!current())return {state:'stale'};
    const context={prompt:initial.sEr(replies),turnTrigger:'send_user_message_async_question',addedFiles:[],fileAttachments:[],ideContext:null,imageAttachments:[]};
    const restoreMessage=initial.Gwt({context,cwd:meta.cwd});
    for(const q of request.questions){const itemKey={...key,itemId:q.id},state=scope.get(initial.g9n,itemKey);scope.set(initial.g9n,itemKey,{...state,isSubmitting:true});}
    let invoked=false;
    try{
      if(!current())return {state:'stale'};
      invoked=true;
      await initial.Jwt({scope,turnTrigger:context.turnTrigger,managerHostId:'local',hostId:'local',targetConversationId:request.threadId,context,cwd:meta.cwd,agentMode:'auto',permissionProfileId:null,shouldSendPermissionOverrides:false,activeCollaborationMode:follow.latestCollaborationMode,restoreMessage,isSubmissionCurrent:current},{queueModeOverride:'send-now'});
      // 与原生提交成功后的处理一致；不覆盖用户正在编辑的回答。
      for(const reply of replies){const itemKey={...key,itemId:reply.questionItemId},state=scope.get(initial.g9n,itemKey);if(state)scope.set(initial.g9n,itemKey,{...state,draft:state.draft.trim()?state.draft:reply.answer,lastSubmission:reply.answer,draftBaseline:reply.answer,isSubmitting:false});}
      const selected=scope.get(initial._9n,{hostId:'local',threadId:request.threadId});
      if(selected?.selectedQuestionKey.entityKey===key.entityKey){
        const remaining=selected.questionIds.filter(id=>{const reply=replies.find(r=>r.questionItemId===id),state=scope.get(initial.g9n,{...key,itemId:id});return !reply||state!=null&&state.draft.trim()!==reply.answer;});
        if(!remaining.length)initial.d9n(scope,selected.selectedQuestionKey);
        else if(typeof initial.b9n==='function')initial.b9n(scope,{hostId:'local',threadId:request.threadId},{...selected,questionIds:remaining,selectedQuestionKey:{...key,itemId:remaining[0]}});
      }
      return {state:'resolved'};
    }catch{return {state:invoked?'uncertain':'unavailable'};}
    finally{
      for(const q of request.questions){const itemKey={...key,itemId:q.id},state=scope.get(initial.g9n,itemKey);if(state?.isSubmitting)scope.set(initial.g9n,itemKey,{...state,isSubmitting:false});}
    }
  }catch{return unavailable();}finally{follow?.[Symbol.dispose]?.();}
}
