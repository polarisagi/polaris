// window.openai 兼容垫片：由服务端（internal/gateway/server/server_apps_view.go）注入 View 文档 <head> 最前，
// 先于 View 自身脚本执行。只实现 openai_mcp_apps.md 表格里能映射到标准桥的子集；未实现的扩展
// （requestCheckout/uploadFile/selectFiles/getFileDownloadUrl/requestModal）故意不定义，让组件特性检测走回退。
// 垫片运行在 View 自己的 window 内（与代理页同源 + allow-same-origin），对宿主请求使用独立 id 前缀
// "openai-shim-"，与 View 自身标准桥的 id 空间不相交。
(function () {
  'use strict';
  var PROTOCOL_VERSION = '2026-01-26';
  var pending = Object.create(null);
  var seq = 0;
  function nextId() { seq += 1; return 'openai-shim-' + seq; }
  function post(msg) { window.parent.postMessage(msg, location.origin); }
  function notify(method, params) { post({ jsonrpc: '2.0', method: method, params: params || {} }); }
  function call(method, params) {
    var id = nextId();
    return new Promise(function (resolve, reject) {
      pending[id] = { resolve: resolve, reject: reject };
      post({ jsonrpc: '2.0', id: id, method: method, params: params || {} });
    });
  }
  function setGlobals(patch) {
    var keys = Object.keys(patch);
    for (var i = 0; i < keys.length; i++) { openai[keys[i]] = patch[keys[i]]; }
    try {
      window.dispatchEvent(new CustomEvent('openai:set_globals', { detail: { globals: patch } }));
    } catch (e) { /* CustomEvent 构造失败的极端环境：忽略，属性已更新 */ }
  }
  function applyHostContext(ctx) {
    if (!ctx || typeof ctx !== 'object') return;
    var patch = {};
    if (ctx.theme !== undefined) patch.theme = ctx.theme;
    if (ctx.displayMode !== undefined) patch.displayMode = ctx.displayMode;
    if (ctx.locale !== undefined) patch.locale = ctx.locale;
    if (ctx.userAgent !== undefined) patch.userAgent = ctx.userAgent;
    if (ctx.containerDimensions && ctx.containerDimensions.maxHeight !== undefined) {
      patch.maxHeight = ctx.containerDimensions.maxHeight;
    }
    if (ctx.safeAreaInsets !== undefined) patch.safeArea = ctx.safeAreaInsets;
    if (Object.keys(patch).length > 0) setGlobals(patch);
  }
  var openai = {
    toolInput: undefined,
    toolOutput: undefined,
    toolResponseMetadata: undefined,
    widgetState: undefined,
    theme: undefined,
    locale: undefined,
    displayMode: 'inline',
    maxHeight: undefined,
    safeArea: undefined,
    userAgent: undefined,
    callTool: function (name, args) { return call('tools/call', { name: name, arguments: args || {} }); },
    sendFollowUpMessage: function (opts) {
      opts = opts || {};
      return call('ui/message', { role: 'user', content: { type: 'text', text: String(opts.prompt || '') } });
    },
    openExternal: function (opts) { opts = opts || {}; return call('ui/open-link', { url: String(opts.href || '') }); },
    requestDisplayMode: function (opts) { opts = opts || {}; return call('ui/request-display-mode', { mode: opts.mode }); },
    setWidgetState: function (state) { openai.widgetState = state; notify('polaris/widget-state', { state: state }); },
    notifyIntrinsicHeight: function (height) {
      notify('ui/notifications/size-changed', {
        width: document.documentElement ? document.documentElement.scrollWidth : 0,
        height: height,
      });
    },
  };
  window.openai = openai;
  window.addEventListener('message', function (event) {
    if (event.source !== window.parent) return;
    if (event.origin !== location.origin) return;
    var msg = event.data;
    if (!msg || msg.jsonrpc !== '2.0') return;
    if (msg.id !== undefined && pending[msg.id]) {
      var p = pending[msg.id];
      delete pending[msg.id];
      if (msg.error) p.reject(msg.error); else p.resolve(msg.result);
      return;
    }
    if (msg.method === 'ui/notifications/tool-input') {
      setGlobals({ toolInput: (msg.params || {}).arguments });
    } else if (msg.method === 'ui/notifications/tool-result') {
      var r = msg.params || {};
      setGlobals({ toolOutput: r.structuredContent, toolResponseMetadata: r._meta });
    } else if (msg.method === 'ui/notifications/host-context-changed') {
      applyHostContext(msg.params);
    }
  });
  call('ui/initialize', {
    capabilities: {},
    clientInfo: { name: 'polaris-openai-compat-shim', version: '1.0.0' },
    protocolVersion: PROTOCOL_VERSION,
    appCapabilities: { availableDisplayModes: ['inline', 'fullscreen'] },
  }).then(function (result) {
    if (result && result.hostContext) applyHostContext(result.hostContext);
    if (result && result._meta && result._meta['polaris/widgetState'] !== undefined) {
      setGlobals({ widgetState: result._meta['polaris/widgetState'] });
    }
    notify('ui/notifications/initialized', {});
  }).catch(function () {
    /* 握手失败：window.openai 保持初始空值，组件应做特性检测/空值回退 */
  });
})();
