// 桌面内部版本兼容入口，不是公开稳定API；独立于创建表单与输入框逻辑。
// 仅接受固定元数据查询和单个thread/archive，不提供任意RPC或模型执行入口。
function desktopArchiveRequest(request) {
  const allowed = new Set(["thread/loaded/list", "thread/read", "thread/archive"]);
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  if (!request || !allowed.has(request.method) || !/^im-agent-archive-[0-9a-f]{32}$/.test(request.id || "")) {
    return Promise.resolve({state:"not_sent"});
  }
  const p = request.params;
  if (!p || typeof p !== "object" || Array.isArray(p)) return Promise.resolve({state:"not_sent"});
  if (request.method === "thread/loaded/list") {
    if (p.limit !== 100 || Object.keys(p).some(k => k !== "limit" && k !== "cursor") ||
        (p.cursor !== undefined && (typeof p.cursor !== "string" || p.cursor.length > 8192))) return Promise.resolve({state:"not_sent"});
  } else {
    if (!uuid.test(p.threadId || "")) return Promise.resolve({state:"not_sent"});
    if (request.method === "thread/archive" && Object.keys(p).some(k => k !== "threadId")) return Promise.resolve({state:"not_sent"});
    if (request.method === "thread/read" && (p.includeTurns !== false || Object.keys(p).some(k => k !== "threadId" && k !== "includeTurns"))) return Promise.resolve({state:"not_sent"});
  }
  if (typeof window.electronBridge?.sendMessageFromView !== "function") return Promise.resolve({state:"not_sent"});
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
      if (response.error) finish({state:"rpc_error", code:response.error.code});
      else if (Object.prototype.hasOwnProperty.call(response,"result")) finish({state:"completed",result:response.result});
      else finish({state:"unknown"});
    };
    const timer = setTimeout(() => finish({state:"unknown"}),7500);
    window.addEventListener("message",receive);
    try {
      Promise.resolve(window.electronBridge.sendMessageFromView({
        type:"mcp-request",hostId:"local",request,
        priority:"critical",retainResponse:false,timeoutMs:7000
      })).catch(() => finish({state:"unknown"}));
    } catch (_) { finish({state:"unknown"}); }
  });
}
