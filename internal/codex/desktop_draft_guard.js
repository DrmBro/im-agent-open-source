// 仅在显式启用草稿保留时调用。只返回给本机桥接，不把草稿放入日志或IM消息。
function desktopDraftGuard(action, token, expected, helpers) {
  const key = "__IM_AGENT_DESKTOP_DRAFT__";
  const sendKey = "__IM_AGENT_DESKTOP_SEND__";
  const policyKey = "__IM_AGENT_DESKTOP_DRAFT_POLICY__";
  // 飞书优先：不再依赖首次注入时可能缺失的输入法事件历史。
  // 可见草稿先加密备份，再通过 draft_settle 释放焦点、复核文本；不伪造 compositionend。
  const {currentThread, composers, logicalText} = helpers;
  const reject = reason => ({ok: false, reason});
  const rawText = el => el.isContentEditable ? logicalText(el) : String(el.value ?? "");
  const plain = el => !el.isContentEditable || el.querySelectorAll("*").length <= 512 && [...el.querySelectorAll("*")].every(node =>
    /^(P|DIV|BR)$/.test(node.nodeName) && node.getAttribute("contenteditable") !== "false" &&
    !node.hasAttribute("data-type") && !node.hasAttribute("data-mention") && !node.hasAttribute("data-composer-code-block"));
  // 使用当前安装包静态资源中确认的结构标记；无法证明没有附件时拒绝暂存。
  const safeRoot = (el, root) => {
    if (!root || !root.isConnected || el.closest("[data-codex-composer-root]") !== root) return false;
    const boxes = root.querySelectorAll("[data-composer-attachments]");
    if (boxes.length !== 1 || root.querySelector("[data-visible-attachments]")) return false;
    if (boxes[0].textContent.trim() || boxes[0].querySelector("img,video,audio,canvas,iframe,[contenteditable],input")) return false;
    if ([...root.querySelectorAll("[data-composer-attachments-row]")].some(row => row.childElementCount > 0)) return false;
    if (root.querySelector("[data-appshot-attachment],[data-appshot-attachment-card],[data-writing-block-image]")) return false;
    if ([...root.querySelectorAll('input[type="file"]')].some(input => input.files && input.files.length > 0)) return false;
    return true;
  };
  // 逻辑偏移使用同一原文序列化，不依赖布局innerText，也不丢弃空行。
  const offsetOf = (el, target, targetOffset) => {
    if (!target || (target !== el && !el.contains(target)) || !Number.isInteger(targetOffset) || targetOffset < 0) return null;
    let length = 0, found = null;
    // 在完整编辑器树中计算偏移，不能把截断后只剩BR的片段误判为占位符。
    const walk = parent => {
      const children = [...parent.childNodes];
      if (parent === target && targetOffset === 0) found = length;
      const meaningful = children.filter(child => child.nodeType !== 8 && (child.nodeType !== 3 || (child.nodeValue || "") !== ""));
      if (meaningful.length === 1 && meaningful[0].nodeName === "BR") {
        if (parent === target && targetOffset <= children.length || children.includes(target) && targetOffset === 0) found = length;
        return;
      }
      let seen = false, previousBlock = false;
      for (let i = 0; i < children.length; i++) {
        const child = children[i];
        if (parent === target && targetOffset === i) found = length;
        if (child.nodeType !== 8 && (child.nodeType !== 3 || (child.nodeValue || "") !== "")) {
          const block = child.nodeType === 1 && /^(DIV|P|PRE|BLOCKQUOTE|LI|H[1-6])$/.test(child.nodeName);
          if (seen && (block || previousBlock)) length++;
          if (child.nodeType === 3) {
            const text = child.nodeValue || "";
            if (child === target && targetOffset <= text.length) found = length + targetOffset;
            length += text.length;
          } else if (child.nodeName === "BR") {
            if (child === target && targetOffset === 0) found = length;
            length++;
          } else walk(child);
          seen = true; previousBlock = block;
        }
        if (parent === target && targetOffset === i + 1) found = length;
      }
    };
    walk(el);
    return length === rawText(el).length ? found : null;
  };
  const saveSelection = el => {
    if (el.nodeName === "TEXTAREA") return {kind: "textarea", start: el.selectionStart, end: el.selectionEnd, direction: el.selectionDirection};
    const selection = window.getSelection();
    if (!selection || typeof selection.setBaseAndExtent !== "function") return null;
    const anchor = offsetOf(el, selection.anchorNode, selection.anchorOffset);
    const focus = offsetOf(el, selection.focusNode, selection.focusOffset);
    return anchor !== null && focus !== null ? {kind: "logical", anchor, focus} : null;
  };
  const pointAt = (el, wanted) => {
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    let node;
    while ((node = walker.nextNode())) {
      const start = offsetOf(el, node, 0);
      if (start !== null && wanted >= start && wanted <= start + node.nodeValue.length) return {node, offset: wanted - start};
    }
    for (const parent of [el, ...el.querySelectorAll("p,div")]) {
      for (const offset of [0, parent.childNodes.length]) {
        if (offsetOf(el, parent, offset) === wanted) return {node: parent, offset};
      }
    }
    return null;
  };
  let state = window[key];
  if (action === "draft_cleanup") {
    if (state && state.token === token) state.cleanup();
    if (window[policyKey]?.token === token) delete window[policyKey];
    return {ok: true};
  }
  if (action === "draft_capture") {
    window[policyKey] = {token, expected};
    if (currentThread() !== expected) return reject("draft_other_thread");
    if (window[sendKey] || state) return reject("draft_busy");
    const inputs = composers();
    if (inputs.length !== 1) return reject("draft_editor_unverified");
    const el = inputs[0], text = rawText(el), root = el.closest("[data-codex-composer-root]");
    // 空文本也可能有图片/附件；不能把用户附件带入飞书的待发消息。
    if (!plain(el) || !safeRoot(el, root)) return reject("draft_unsupported");
    window[policyKey].sendValid = () => el.isConnected && currentThread() === expected &&
      composers().length === 1 && composers()[0] === el && plain(el) && safeRoot(el, root);
    if (text === "") return {ok: true, empty: true};
    if (new TextDecoder().decode(new TextEncoder().encode(text)) !== text) return reject("draft_unsupported");
    if (el.disabled || el.readOnly) return reject("draft_unsupported");
    if (new TextEncoder().encode(text).length > 65536) return reject("draft_too_large");
    if (el.isContentEditable) {
      const lines = text.split("\n");
      const restoredElements = lines.length + lines.filter(line => line === "").length;
      if (restoredElements > 512) return reject("draft_structure_budget");
    }
    state = {
      token, expected, el, root, text, focus: document.activeElement,
      blocked: "", internal: false, cleared: false, restoreArmed: false,
      remoteBefore: false, restoreBefore: false, restoreInserted: false, applyingRestore: false,
      selection: saveSelection(el)
    };
    state.baseValid = () => state.el.isConnected && currentThread() === expected &&
      composers().length === 1 && composers()[0] === state.el && safeRoot(state.el, state.root) &&
      !state.el.disabled && !state.el.readOnly;
    // 原发送事务仍执行其原有校验；保留草稿时额外拒绝期间发生过的用户操作。
    state.sendValid = () => state.cleared && !state.blocked && !state.restoreArmed && state.baseValid();
    const stopInsertion = (event, reason) => {
      state.blocked = reason;
      event.preventDefault();
      event.stopImmediatePropagation();
    };
    const observe = event => {
      if (state.internal) return;
      const tx = window[sendKey];
      const ours = tx && tx.token === token;
      if (ours && tx.rollingBack) return;
      if (event.type === "beforeinput") {
        // 恢复在同一次同步页面调用内执行；兼容原生纯文本HTML插入的beforeinput类型。
        if (state.applyingRestore && state.restoreArmed) {
          if (event.target !== el || state.blocked || !state.baseValid() || document.activeElement !== el || !plain(el) || rawText(el) !== "") {
            stopInsertion(event, "draft_restore_conflict");
          } else state.restoreBefore = true;
          return;
        }
        if (state.restoreArmed && event.inputType === "insertText" && event.data === text) {
          if (event.target !== el || state.blocked || !state.baseValid() || document.activeElement !== el || !plain(el) || rawText(el) !== "") {
            stopInsertion(event, "draft_restore_conflict");
          } else {
            state.restoreBefore = true;
          }
          return;
        }
        if (ours && !tx.inserted && event.target === el && event.inputType === "insertText" && event.data === tx.prompt) {
          state.remoteBefore = true;
          return;
        }
      }
      if (event.type === "input" && event.target === el) {
        if (state.applyingRestore && !state.blocked && state.baseValid() && rawText(el) === text) {
          state.restoreInserted = true;
          state.restoreBefore = false;
          return;
        }
        if (state.restoreBefore) {
          state.restoreBefore = false;
          state.restoreInserted = true;
          return;
        }
        if (state.remoteBefore) {
          state.remoteBefore = false;
          return;
        }
        // 桌面处理刚才的Enter后可能清空输入；用户键盘操作已由keydown单独记录。
        if (ours && tx.enterDelivered && rawText(el) === "") return;
      }
      if (event.type === "keydown" && event.key === "Enter" && ours && tx.inserted && !tx.enterDelivered) return;
      state.blocked = "draft_user_changed";
    };
    const events = ["beforeinput", "input", "keydown", "pointerdown", "paste", "drop", "compositionstart", "focusin"];
    events.forEach(type => document.addEventListener(type, observe, true));
    const blur = () => { if (!state.internal) state.blocked = "draft_window_changed"; };
    window.addEventListener("blur", blur, true);
    state.cleanup = () => {
      events.forEach(type => document.removeEventListener(type, observe, true));
      window.removeEventListener("blur", blur, true);
      clearTimeout(state.timer);
      if (window[key] === state) delete window[key];
    };
    state.timer = setTimeout(state.cleanup, 150000);
    window[key] = state;
    return {ok: true, text};
  }
  if (!state || state.token !== token || state.expected !== expected) return reject("draft_expired");
  if (action === "draft_send_ready") {
    return state.sendValid() && plain(state.el) && rawText(state.el) === "" && document.activeElement === state.el ? {ok: true} : reject("draft_capture_conflict");
  }
  if (action === "draft_settle") {
    // 宿主只有在当前精确原文已加密落盘后才能调用。先校验，绝不按相似文本猜测。
    if (state.cleared || state.blocked || !state.baseValid() || !plain(state.el) || rawText(state.el) !== state.text || document.activeElement !== state.focus) {
      return reject("draft_capture_conflict");
    }
    state.internal = true;
    try {
      // 让真实编辑器处理已有候选，不发送 Enter，也不伪造输入法状态。
      state.el.blur();
    } finally {
      state.internal = false;
    }
    if (!state.baseValid() || !plain(state.el)) return reject("draft_capture_conflict");
    if (rawText(state.el) !== state.text) {
      // 原快照不删除；变化后的文本必须由宿主重新备份后才允许移走。
      state.cleanup();
      return {ok: true, recapture: true};
    }
    state.focus = document.activeElement;
    state.settled = true;
    return {ok: true};
  }
  if (action === "draft_take") {
    if (!state.settled || state.cleared || state.blocked || !state.baseValid() || !plain(state.el) || rawText(state.el) !== state.text || document.activeElement !== state.focus) {
      return reject("draft_capture_conflict");
    }
    state.internal = true;
    try {
      state.el.focus();
      if (!state.baseValid() || document.activeElement !== state.el || rawText(state.el) !== state.text) return reject("draft_capture_conflict");
      if (state.el.isContentEditable) {
        const range = document.createRange(); range.selectNodeContents(state.el);
        const selection = window.getSelection();
        if (!selection) return reject("draft_editor_unverified");
        selection.removeAllRanges(); selection.addRange(range);
        // 某些contenteditable对首尾空行执行delete后会残留真实换行。
        // 在同一已验证选区事务中用单个占位BR替换，避免遗留额外块级包装，并精确读为空。
        if (!document.execCommand("insertHTML", false, "<br>")) return reject("draft_clear_unconfirmed");
      } else if (state.el.nodeName === "TEXTAREA") {
        const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value").set;
        setter.call(state.el, "");
        state.el.dispatchEvent(new Event("input", {bubbles: true}));
      } else return reject("draft_unsupported");
      state.cleared = state.baseValid() && plain(state.el) && rawText(state.el) === "";
      return state.cleared ? {ok: true} : reject("draft_clear_unconfirmed");
    } finally {
      state.internal = false;
    }
  }
  if (action === "draft_restore_prepare") {
    if (state.blocked) return reject("draft_restore_blocked");
    if (!state.baseValid()) return reject("draft_restore_target_changed");
    if (!plain(state.el)) return reject("draft_restore_structure_changed");
    const text = rawText(state.el);
    // 相同文本可能只是尚未清空的飞书待发内容，不能当作恢复证明。
    if (!state.cleared && text === state.text) return {ok: true, already: true};
    if (text !== "") return reject("draft_wait_empty");
    if (document.activeElement !== state.el) return reject("draft_restore_focus_changed");
    state.restoreArmed = true;
    return {ok: true};
  }
  if (action === "draft_restore_apply") {
    // 核验和插入在同一次页面任务中完成，避免跨CDP调用时文档重载后把草稿注入新页面。
    if (!state.restoreArmed) return reject("draft_restore_not_armed");
    if (state.blocked) return reject("draft_restore_blocked");
    if (!state.baseValid()) return reject("draft_restore_target_changed");
    if (!plain(state.el)) return reject("draft_restore_structure_changed");
    if (rawText(state.el) !== "") return reject("draft_restore_not_empty");
    if (document.activeElement !== state.el) return reject("draft_restore_focus_changed");
    state.applyingRestore = true;
    try {
      // 空编辑器可能仍有占位BR，旧选区也可能停在它之后。替换整个已验证的空内容，
      // 避免前导换行插入时把占位BR变成多余的真实空行；绝不裁剪恢复原文。
      if (state.el.isContentEditable) {
        const selection = window.getSelection();
        if (!selection) return reject("draft_restore_selection_unavailable");
        const range = document.createRange(); range.selectNodeContents(state.el);
        selection.removeAllRanges(); selection.addRange(range);
      } else state.el.setSelectionRange(0, 0);
      if (state.blocked) return reject("draft_restore_blocked");
      if (!state.baseValid()) return reject("draft_restore_target_changed");
      if (rawText(state.el) !== "") return reject("draft_restore_not_empty");
      if (document.activeElement !== state.el) return reject("draft_restore_focus_changed");
      if (state.el.isContentEditable) {
        // 浏览器insertText对前导换行可能生成额外BR。只构造转义后的纯文本行，
        // 显式空行使用占位BR；不接收草稿HTML，也不裁剪原文或吞掉真实空行。
        const escape = text => text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/\r/g, "&#13;").replace(/\t/g, "&#9;");
        const html = state.text.split("\n").map(line => "<div>" + (line === "" ? "<br>" : escape(line)) + "</div>").join("");
        if (!document.execCommand("insertHTML", false, html)) return reject("draft_restore_command_failed");
      } else if (!document.execCommand("insertText", false, state.text)) return reject("draft_restore_command_failed");
    } finally { state.applyingRestore = false; }
    if (state.blocked) return reject("draft_restore_blocked");
    if (!state.baseValid()) return reject("draft_restore_target_changed");
    if (!state.restoreInserted) return reject("draft_restore_input_unobserved");
    if (rawText(state.el) !== state.text) return reject("draft_restore_content_mismatch");
    return {ok: true};
  }
  if (action === "draft_restore_confirm") {
    if (state.blocked) return reject("draft_restore_blocked");
    if (!state.restoreInserted) return reject("draft_restore_input_unobserved");
    if (!state.baseValid()) return reject("draft_restore_target_changed");
    if (!plain(state.el)) return reject("draft_restore_structure_changed");
    if (rawText(state.el) !== state.text) return reject("draft_restore_content_mismatch");
    if (state.selection && document.activeElement === state.el) {
      state.internal = true;
      try {
        if (state.selection.kind === "textarea") {
          state.el.setSelectionRange(state.selection.start, state.selection.end, state.selection.direction);
        } else {
          const anchor = pointAt(state.el, state.selection.anchor), focus = pointAt(state.el, state.selection.focus);
          if (anchor && focus) window.getSelection()?.setBaseAndExtent(anchor.node, anchor.offset, focus.node, focus.offset);
        }
      } finally { state.internal = false; }
    }
    return {ok: true};
  }
  return reject("draft_invalid_action");
}
