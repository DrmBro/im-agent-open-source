// 使用已安装桌面的 preload / 主进程消息协议，不伪造 HTTP 客户端身份。
// 仅允许创建表单需要的两个元数据操作，不提供通用 RPC、turn/start 或命令入口。
function desktopThreadRequest(request) {
  const allowed = new Set(["thread/start", "thread/name/set"]);
  if (!request || !allowed.has(request.method) || typeof request.id !== "string" || !request.id.startsWith("im-agent-create-")) {
    return Promise.resolve({state:"rejected"});
  }
  if (typeof window.electronBridge?.sendMessageFromView !== "function") {
    return Promise.resolve({state:"unavailable"});
  }
  return new Promise(resolve => {
    let finished = false;
    const finish = value => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      window.removeEventListener("message", receive);
      resolve(value);
    };
    const receive = event => {
      if (event.source != null && event.source !== window) return;
      const data = event.data;
      if (data?.type !== "mcp-response" || data.hostId !== "local" || data.message?.id !== request.id) return;
      const response = data.message;
      if (response.error) {
        finish({state:"rpc_error",code:response.error.code});
      } else if (Object.prototype.hasOwnProperty.call(response,"result")) {
        finish({state:"completed",result:response.result});
      } else {
        finish({state:"uncertain"});
      }
    };
    const timer = setTimeout(()=>finish({state:"uncertain"}),7500);
    window.addEventListener("message",receive);
    try {
      Promise.resolve(window.electronBridge.sendMessageFromView({
        type:"mcp-request",hostId:"local",request,
        priority:"critical",retainResponse:false,timeoutMs:7000
      })).catch(()=>finish({state:"uncertain"}));
    } catch (_) {finish({state:"uncertain"});}
  });
}
