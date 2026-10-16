// 使用已安装桌面与 CodexPlusPlus 源码确认的 DOM 标记；只为保护草稿读取目标编辑器文本，不读历史消息或凭证。
// 由桥接作为带参数的表达式调用；独立函数便于在模拟 DOM 中验证，无须接触实际桌面。
function desktopGuard(action, token, threadId, prompt, draftGuard, requireDraft) {
  const key = "__IM_AGENT_DESKTOP_SEND__";
  const navKey = "__IM_AGENT_DESKTOP_NAVIGATION__";
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  const normalize = value => {
    const id = String(value || "").replace(/^local:/i, "");
    return uuid.test(id) ? id.toLowerCase() : "";
  };
  const visible = el => {
    const r = el.getBoundingClientRect(), style = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && style.visibility !== "hidden" && style.display !== "none";
  };
  const currentThread = () => {
    const ids = new Set();
    for (const row of document.querySelectorAll("[data-app-action-sidebar-thread-id]")) {
      if (!visible(row)) continue;
      const selected = row.getAttribute("aria-current") === "page" ||
        row.getAttribute("data-app-action-sidebar-thread-selected") === "true" ||
        row.getAttribute("aria-selected") === "true";
      if (!selected) continue;
      const id = normalize(row.getAttribute("data-app-action-sidebar-thread-id"));
      // 当前选中的临时会话也算冲突，不能静默改用旧 URL。
      if (!id) return "";
      ids.add(id);
    }
    let route;
    try { route = decodeURIComponent(location.pathname + location.search + location.hash); }
    catch { return ""; }
    const match = route.match(/(?:\/(?:local|thread|threads|conversation|session)\/|[?&](?:threadId|conversationId|sessionId)=)([0-9a-f-]{36})(?=[/?&#]|$)/i);
    if (match) ids.add(normalize(match[1]));
    return ids.size === 1 ? [...ids][0] : "";
  };
  const editableCandidates = () => [...document.querySelectorAll('textarea,[contenteditable="true"]')]
    .filter(visible)
    .filter((el, i, all) => !all.some((other, j) => i !== j && other.contains(el)));
  // 不再要求整个页面只有一个输入框。已识别聊天区域内的目标不受外部编辑区数量影响。
  // 只使用安装包中已确认的聊天区域标记；旧版无此标记且只有一个编辑器时仍兼容。
  const selectComposer = () => {
    const inputs = editableCandidates();
    const markedRoots = [...document.querySelectorAll("[data-codex-composer-root]")];
    if (markedRoots.length > 0) {
      // 有标记但聊天区域隐藏/尚未加载，不是旧版无标记，不能回退到其它面板。
      const roots = markedRoots.filter(visible);
      const scoped = inputs.filter(el => roots.includes(el.closest("[data-codex-composer-root]")));
      if (scoped.length === 1) return {el: scoped[0], root: scoped[0].closest("[data-codex-composer-root]")};
      return {el: null, reason: scoped.length === 0 ? "composer_missing" : "composer_target_ambiguous"};
    }
    if (inputs.length === 1) return {el: inputs[0], root: null};
    return {el: null, reason: inputs.length === 0 ? "composer_missing" : "composer_target_ambiguous"};
  };
  // 草稿处理也只收到解析后的聊天目标，不把旁边的编辑区视为另一个聊天框。
  const composers = () => {
    const selected = selectComposer();
    return selected.el ? [selected.el] : [];
  };
  // innerText 是布局文本：空 div/br 会额外增加空行，连续空格会变成 NBSP。
  // 读取编辑节点的逻辑行，不折叠空行、缩进或制表符，不改写真正提交的文本。
  const normalizeText = text => String(text ?? "").replace(/\r\n?/g, "\n").replace(/\u00a0/g, " ").trim();
  const logicalText = (parent, preserveDraft = false) => {
    // 草稿兼容模式忽略没有字符的编辑器占位节点；不忽略空格、换行或其它真实字符。
    const children = [...parent.childNodes].filter(child => !preserveDraft || child.nodeType !== 8 && (child.nodeType !== 3 || (child.nodeValue || "") !== ""));
    if (children.length === 1 && children[0].nodeName === "BR") return "";
    let out = "", seen = false, previousBlock = false;
    for (const child of children) {
      if (child.nodeType === 8) continue;
      const block = child.nodeType === 1 && /^(DIV|P|PRE|BLOCKQUOTE|LI|H[1-6])$/.test(child.nodeName);
      let value;
      if (child.nodeType === 3) value = child.nodeValue || "";
      else if (child.nodeName === "BR") value = "\n";
      else value = logicalText(child, preserveDraft);
      if (seen && (block || previousBlock)) out += "\n";
      out += value;
      seen = true;
      previousBlock = block;
    }
    return out;
  };
  const draftRequired = requireDraft === true;
  const strictDraft = () => draftRequired || window.__IM_AGENT_DESKTOP_DRAFT_POLICY__?.token === token;
  const textOf = el => {
    const raw = String(el.isContentEditable ? logicalText(el, strictDraft()) : el.value ?? "");
    return strictDraft() ? raw : normalizeText(raw);
  };
  const matchesPrompt = (el, text) => textOf(el) === (strictDraft() ? String(text) : normalizeText(text));
  const tx = window[key];
  if (action === "cleanup") {
    if (tx && tx.token === token) tx.cleanup();
    const navigation = window[navKey];
    if (navigation && navigation.token === token) {
      clearTimeout(navigation.timer);
      delete window[navKey];
    }
    return {ok: true};
  }
  const expected = normalize(threadId);
  if (!expected) return {ok: false, reason: "invalid_thread"};
  if (action.startsWith("draft_")) {
    if (typeof draftGuard !== "function") return {ok: false, reason: "draft_handler_unavailable"};
    return draftGuard(action, token, expected, {currentThread, composers, logicalText: parent => logicalText(parent, true)});
  }
  // 宿主明确要求保留模式时，页面状态丢失不能被误当成关闭开关。
  if (draftRequired && window.__IM_AGENT_DESKTOP_DRAFT_POLICY__?.token !== token) return {ok: false, reason: "draft_policy_lost"};
  if (action === "navigate" || action === "navigate_external" || action === "navigate_target") {
    if (tx) return {ok:false,reason:"submission_busy"};
    let navigation = window[navKey];
    if (navigation && navigation.token !== token) return {ok:false,reason:"navigation_busy"};
    const current = currentThread();
    if (action === "navigate_target" && current !== expected) return {ok:false,reason:"navigation_interrupted"};
    if (navigation && navigation.arrived && current !== expected) {
      return {ok:false,reason:"navigation_interrupted"};
    }
    if (navigation && current &&
        current !== expected && current !== navigation.from) {
      return {ok:false,reason:"navigation_interrupted"};
    }
    // 只点击一次。点击后必须等路由/侧栏确认目标，不重复抢回用户手动切换的窗口。
    if (navigation && navigation.clicked && current !== expected) {
      return {ok:false,reason:"navigation_pending"};
    }
    if (current === expected) {
      if (navigation && navigation.token === token) {
        clearTimeout(navigation.timer);
        delete window[navKey];
      }
      const selected = selectComposer();
      if (!selected.el) {
        if (selected.reason === "composer_missing") return {ok:false,reason:"waiting_composer"};
        return {ok:false,reason:selected.reason};
      }
      const el = selected.el;
      if (el.disabled || el.readOnly) return {ok:false,reason:"waiting_composer"};
      if (textOf(el)) return {ok:false,reason:"waiting_draft"};
      return {ok:true};
    }
    const heldDraft = window.__IM_AGENT_DESKTOP_DRAFT__;
    if (heldDraft && heldDraft.token === token) return {ok:false,reason:"navigation_interrupted"};
    const rows = [...document.querySelectorAll("[data-app-action-sidebar-thread-id]")]
      .filter(visible)
      .filter(row => normalize(row.getAttribute("data-app-action-sidebar-thread-id")) === expected);
    if (!rows.length && action !== "navigate_external") return {ok:false,reason:"navigation_target_missing"};
    const row = rows[0];
    if (row && (row.getAttribute("aria-disabled") === "true" || row.disabled || typeof row.click !== "function")) {
      return {ok:false,reason:"navigation_target_missing"};
    }
    if (!navigation) {
      navigation = {token, from:current, clicked:false, arrived:false, timer:null};
      navigation.timer = setTimeout(() => {
        if (window[navKey] === navigation) delete window[navKey];
      }, 30000);
      window[navKey] = navigation;
    }
    if (!rows.length) {
      // 只预留一次外部导航；链接由宿主打开，页面内不猜路由、不注入历史。
      navigation.clicked = true;
      navigation.from = current;
      return {ok:false,reason:"external_navigation_ready"};
    }
    navigation.clicked = true;
    navigation.from = current;
    row.click();
    return {ok:false,reason:"navigation_pending"};
  }
  if (action === "prepare") {
    if (tx) return {ok: false, reason: "submission_busy"};
    if (currentThread() !== expected) return {ok: false, reason: "thread_unverified"};
    const selected = selectComposer();
    if (!selected.el) return {ok: false, reason: selected.reason};
    const el = selected.el;
    if (el.disabled || el.readOnly || textOf(el)) return {ok: false, reason: "composer_has_draft"};
    const state = {token, el, prompt, inserted: false, enterDelivered: false, blocked: "", timer: null};
    const heldDraft = window.__IM_AGENT_DESKTOP_DRAFT__;
    const heldPolicy = window.__IM_AGENT_DESKTOP_DRAFT_POLICY__;
    const sameComposer = () => {
      const current = selectComposer();
      return current.el === el && current.root === selected.root;
    };
    state.userInteracted = false;
    state.issue = () => {
      if (!el.isConnected) return "composer_detached";
      if (currentThread() !== expected) return "thread_changed";
      if (!sameComposer()) return "composer_replaced";
      if (state.userInteracted) return "user_interaction";
      if (heldDraft && heldDraft.token === token && !heldDraft.sendValid()) return "draft_state_changed";
      if (draftRequired && (!heldPolicy || heldPolicy.token !== token || window.__IM_AGENT_DESKTOP_DRAFT_POLICY__ !== heldPolicy)) return "draft_policy_lost";
      if (heldPolicy && heldPolicy.token === token && (typeof heldPolicy.sendValid !== "function" || !heldPolicy.sendValid())) return "draft_state_changed";
      if (document.activeElement !== el) return "focus_changed";
      return "";
    };
    state.valid = () => state.issue() === "";
    // 仅恢复无用户交互且内容未变的同一编辑器焦点，不重建/替换编辑器。
    state.restoreFocus = () => {
      if (state.blocked || state.issue() !== "focus_changed") return;
      if (state.inserted ? !matchesPrompt(el, prompt) : textOf(el) !== "") return;
      // 草稿观察者也需区分这次经过校验的程序聚焦和真实用户焦点变化。
      const draft = heldDraft && heldDraft.token === token ? heldDraft : null;
      const internalBefore = draft?.internal;
      if (draft) draft.internal = true;
      try { el.focus({preventScroll: true}); }
      finally { if (draft) draft.internal = internalBefore; }
    };
    const observeInteraction = event => {
      if (!event.isTrusted || state.enterDelivered) return;
      // 本次原生输入和待发送Enter由既有 beforeInput/keyDown 处理。
      if (event.type === "keydown" && event.key === "Enter" && event.target === el && state.inserted) return;
      state.userInteracted = true;
    };
    const interactionEvents = ["pointerdown", "paste", "drop", "compositionstart", "keydown"];
    const block = (event, reason) => {
      state.blocked = reason;
      event.preventDefault();
      event.stopImmediatePropagation();
    };
    const beforeInput = event => {
      if (event.inputType === "insertText" && event.data === prompt && !state.inserted) {
        if (event.target === el) state.restoreFocus();
        if (!state.valid() || event.target !== el || state.blocked || textOf(el)) {
          block(event, state.blocked || state.issue() || (event.target !== el ? "input_target_changed" : "composer_has_draft")); return;
        }
        state.inserted = true;
      } else if (!state.rollingBack && !state.enterDelivered) {
        // 用户同时编辑时取消桥接发送，不阻止用户自己的编辑。
        state.blocked = "composer_changed";
      }
    };
    const keyDown = event => {
      if (event.key !== "Enter") return;
      // Enter事件已分派时不可重新聚焦/重发；这里保留失败而非伪造回执。
      if (!state.valid() || event.target !== el || !state.inserted || state.blocked || !matchesPrompt(el, prompt)) {
        block(event, state.blocked || state.issue() || (event.target !== el ? "input_target_changed" : "composer_content_mismatch")); return;
      }
      state.enterDelivered = true;
    };
    state.cleanup = () => {
      document.removeEventListener("beforeinput", beforeInput, true);
      document.removeEventListener("keydown", keyDown, true);
      interactionEvents.forEach(type => document.removeEventListener(type, observeInteraction, true));
      clearTimeout(state.timer);
      if (window[key] === state) delete window[key];
    };
    window[key] = state;
    document.addEventListener("beforeinput", beforeInput, true);
    document.addEventListener("keydown", keyDown, true);
    interactionEvents.forEach(type => document.addEventListener(type, observeInteraction, true));
    state.timer = setTimeout(state.cleanup, 30000);
    el.focus();
    if (!state.valid()) {const reason = state.issue(); state.cleanup(); return {ok:false,reason};}
    return {ok:true};
  }
  if (!tx || tx.token !== token) return {ok:false,reason:"submission_expired"};
  if (action === "rollback") {
    // 只有本事务从空编辑器插入、尚未提交且内容完全未改动，才允许删除本程序的草稿。
    // 旧版本遗留/用户手动编辑/会话或焦点改变/已按 Enter 的内容一概保留。
    if (!tx.inserted || tx.enterDelivered || tx.blocked || !tx.valid() || !matchesPrompt(tx.el, tx.prompt)) {
      return {ok:true,cleared:false,retained:textOf(tx.el)!==""};
    }
    tx.rollingBack = true;
    try {
      if (tx.el.isContentEditable) {
        const range = document.createRange();range.selectNodeContents(tx.el);
        const selection = window.getSelection();selection.removeAllRanges();selection.addRange(range);
        document.execCommand("delete", false);
      } else {
        const prototype = tx.el.tagName === "TEXTAREA" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        Object.getOwnPropertyDescriptor(prototype,"value").set.call(tx.el, "");
        tx.el.dispatchEvent(new Event("input",{bubbles:true}));
      }
    } finally {tx.rollingBack = false;}
    return {ok:true,cleared:textOf(tx.el)==="",retained:textOf(tx.el)!==""};
  }
  if (tx.blocked) return {ok:false,reason:tx.blocked};
  if (action === "focus_ready") {
    tx.restoreFocus();
    if (!tx.valid()) return {ok:false,reason:tx.issue()};
    if (tx.inserted || textOf(tx.el)) return {ok:false,reason:"composer_has_draft"};
    return {ok:true};
  }
  if (action === "ready") {
    tx.restoreFocus();
    if (!tx.valid()) return {ok:false,reason:tx.issue()};
    if (!tx.inserted) return {ok:false,reason:"insertion_unconfirmed"};
    if (!matchesPrompt(tx.el,tx.prompt)) return {ok:false,reason:"composer_content_mismatch"};
    return {ok:true};
  }
  if (action === "receipt") return {ok:tx.enterDelivered,reason:"submission_unconfirmed"};
  return {ok:false,reason:"invalid_action"};
}
