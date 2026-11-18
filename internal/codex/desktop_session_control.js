// 只允许会话控制协议，不操作输入框和草稿。
function desktopSessionControl(request) {
 const allowed=new Set(["thread/read","thread/resume","thread/turns/list","turn/start","turn/interrupt","turn/steer"]);
 if(!request||!allowed.has(request.method)||!request.id?.startsWith("im-agent-control-")||typeof request.params?.threadId!=="string"||!request.params.threadId)return Promise.resolve({state:"rejected"});
 if(request.method==="turn/steer"&&(!request.params.expectedTurnId||!Array.isArray(request.params.input)))return Promise.resolve({state:"rejected"});
 if(request.method==="turn/interrupt"&&!request.params.turnId)return Promise.resolve({state:"rejected"});
 if(typeof window.electronBridge?.sendMessageFromView!=="function")return Promise.resolve({state:"unavailable"});
 return new Promise(resolve=>{
  let finished=false;
  const finish=v=>{if(finished)return;finished=true;clearTimeout(timer);window.removeEventListener("message",receive);resolve(v);};
  const receive=event=>{
   if(event.source!=null&&event.source!==window)return;
   const data=event.data;if(data?.type!=="mcp-response"||data.hostId!=="local"||data.message?.id!==request.id)return;
   const response=data.message;if(response.error){const message=String(response.error.message??'').toLowerCase();
    const reason=/thread.{0,50}(not found|does not exist)|no thread found/.test(message)?'not_found':response.error.code===-32602?'invalid_params':/unauthorized|authentication|sign in/.test(message)?'authentication':'rejected';
    finish({state:"rpc_error",code:response.error.code,reason});return;}
   if(!Object.prototype.hasOwnProperty.call(response,"result")){finish({state:"uncertain"});return;}
   let result=response.result;
   // 只返回公开助手文本，推理与工具结果不离开桌面进程。
   if(request.method==="thread/turns/list"&&Array.isArray(result?.data))result={nextCursor:result.nextCursor??null,data:result.data.map(t=>{
    const items=Array.isArray(t.items)?t.items.filter(i=>i.type==="agentMessage"&&(i.phase==null||i.phase===""||i.phase==="commentary"||i.phase==="final_answer")&&typeof i.text==="string").map(i=>({id:i.id,type:i.type,phase:i.phase,status:i.status,text:i.text})):null;
    return {id:t.id,status:t.status,itemsView:t.itemsView,items,error:t.error?{message:"该轮次返回错误"}:null,preview:(items||[]).slice(-3).map(i=>i.text).join("\n\n").slice(-1200)};
   })};
   if(request.method==="thread/read"||request.method==="thread/resume")result={thread:{id:result?.thread?.id,model:result?.thread?.model,status:result?.thread?.status}};
   if(request.method==="turn/steer")result={turnId:result?.turnId};
   if(request.method==="turn/start")result={turn:{id:result?.turn?.id,status:result?.turn?.status}};
   finish({state:"completed",result});
  };
  const timer=setTimeout(()=>finish({state:"uncertain"}),7500);window.addEventListener("message",receive);
  try{Promise.resolve(window.electronBridge.sendMessageFromView({type:"mcp-request",hostId:"local",request,priority:"critical",retainResponse:false,timeoutMs:7000})).catch(()=>finish({state:"uncertain"}));}catch(_){finish({state:"uncertain"});}
 });
}
