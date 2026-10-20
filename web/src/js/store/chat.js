import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
import { SSEClient, dedupeRunID } from '../sse.js'
import { mcpAppsHost } from '../mcp_apps.js'
import { WavRecorder } from '../audio/wav_recorder.js'
// ══════════════════════════════════════════════════════════════════════════
// store: chat（主对话状态机）
// ══════════════════════════════════════════════════════════════════════════
// ── 多会话运行时（ADR-0116：回合与连接解耦，会话可后台运行、多会话并行）──────────
// runs：每个运行中/刚结束的回合一份状态，键为 sessionID（新会话在收到 session 事件前
// 用临时键）。视图字段（state/currentTokens/…）是对「当前视图所属 run」的访问器：
// 事件处理器写入各自的 run，只有视图所属的 run 才会驱动可见界面。
// SSEClient 含私有字段，经 Alpine 响应式代理调用会抛 TypeError，故放模块级 Map。
const sseClients = new Map() // run.rid -> SSEClient

// 视图字段默认值（视图无 run 时读写 _idle）
const idleDefaults = () => ({
  state: 'IDLE', taskID: null, currentTokens: '', thinkingText: '', errorMsg: '',
  contextWarning: null, compacting: false, phase: '', approvals: [], subagents: [],
})
const RUN_FIELDS = Object.keys(idleDefaults())

function newRun(key, sessionID, messages) {
  return {
    rid: crypto.randomUUID(),
    key,                 // runs 中的键
    sessionID,           // 新会话在 session 事件前为 null
    messages,            // 该会话的消息数组；run 在视图中时与 store.messages 是同一个数组
    ...idleDefaults(),
    state: 'SUBMITTING',
    pendingToolCall: null,
    aborting: false,     // 用户已点中断，等服务端终止事件
    abortRequested: false, // 点中断时尚不知道 sessionID，待 session 事件后补发
    finished: false,
    title: '',
  }
}

const chatStore = {
  // 每个 run 的状态机: SUBMITTING | THINKING | STREAMING | TOOL_RUNNING | COMPLETE | ERROR
  runs: {},
  viewKey: null,         // 当前视图所属 run 的键；null = 无运行中回合的纯浏览视图
  _idle: idleDefaults(),
  _loadSeq: 0,
  sessionID: null,
  messages: [],          // [{role, content, toolCalls, aborted}]
  thinkingOpen: true,
  _historyIdx: -1,
  _inputHistory: [],     // 初始化输入历史数组，防止首次加载时未定义报错
  attachments: [],       // [{ uri, mime_type, name, dataUrl }]
  capabilities: null,
  ttsEnabled: false,
  isRecording: false,
  _wavRecorder: null,
  _checkVADTimeout: null,
  _mediaRecorder: null,
  _audioChunks: [],
  lastAbortedInput: null,  // 上次被中断的用户输入内容，用于恢复编辑按钮
  selectedModel: '',
  reasoningEffort: 'auto',

  get viewRun() { return this.viewKey ? (this.runs[this.viewKey] || null) : null },
  get isActive() { return this.state !== 'IDLE' && this.state !== 'COMPLETE' && this.state !== 'ERROR' },

  // 会话是否有仍在运行的本地回合（侧栏/会话页/项目页的脉动圆点；与服务端 running 字段取并集）
  isSessionRunning(id) {
    const r = id ? this.runs[id] : null
    return !!(r && !r.finished && !r.aborting)
  },
  // 有回合正在后台（非当前视图）运行
  get backgroundRunning() {
    return Object.values(this.runs).some(r => !r.finished && !r.aborting && r.key !== this.viewKey)
  },

  async fetchCapabilities() {
    try {
      const res = await fetch('/v1/system/capabilities', { headers: authHeaders() })
      if (res.ok) {
        this.capabilities = await res.json()
      }
    } catch (e) {
      console.warn('Failed to fetch system capabilities:', e)
    }
  },

  toggleTTS() {
    this.ttsEnabled = !this.ttsEnabled;
  },

  toggleThinking() {
    this.thinkingOpen = !this.thinkingOpen;
  },

  async uploadFile(file) {
    // Generate a local preview dataUrl if it's an image
    let dataUrl = null;
    if (file.type.startsWith('image/')) {
      dataUrl = await new Promise((resolve) => {
        const reader = new FileReader();
        reader.onload = (e) => resolve(e.target.result);
        reader.readAsDataURL(file);
      });
    }

    const formData = new FormData();
    formData.append('file', file);
    try {
      // Create headers but remove Content-Type so fetch can auto-set the boundary for multipart/form-data
      const headers = authHeaders();
      delete headers['Content-Type'];

      const resp = await fetch('/v1/workspace/upload', {
        method: 'POST',
        headers: headers,
        body: formData
      });
      if (resp.ok) {
        const data = await resp.json();
        this.attachments.push({
          uri: data.uri,
          mime_type: data.mime_type,
          name: data.name,
          dataUrl: dataUrl
        });
      } else {
        throw new Error('Upload failed with status: ' + resp.status);
      }
    } catch (e) {
      console.error("Upload failed", e);
      if (Alpine.store('toast')) {
        Alpine.store('toast').show('error', `Failed to upload ${file.name}`);
      } else {
        Alpine.store('dialog').alert({ message: `Failed to upload ${file.name}` });
      }
    }
  },

  removeAttachment(index) {
    this.attachments.splice(index, 1);
  },

  async toggleRecording() {
    if (this.isRecording) {
      await this._stopRecording();
      return;
    }

    // 开麦前先确认语音识别资产可用：未安装则征得同意后按需下载，不支持则说明最低配置。
    // 否则用户说完话只会得到一个 503，必须在开麦前就告知状态与原因。
    if (!(await this._ensureAudioAsset('stt'))) return;

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      window.dispatchEvent(new CustomEvent('stt-start'));

      const recorder = new WavRecorder(stream, 16000);
      await recorder.init();
      this._wavRecorder = recorder;

      let silenceStart = null;
      // 优化后的 VAD 参数：阈值 8 减少底噪触发，停顿 1200ms 允许自然换气与思考
      const SILENCE_THRESHOLD = 8;
      const SHORT_PAUSE_MS = 1200;
      const LONG_PAUSE_MS = 4000;

      let isSpeaking = false;
      let chunkErrorShown = false; // 同一次录音内分段失败只弹一次 toast，避免刷屏

      const uploadChunk = async (blob) => {
        const formData = new FormData();
        formData.append('file', blob, 'chunk.wav');
        try {
          const headers = authHeaders();
          delete headers['Content-Type'];
          const resp = await fetch('/v1/audio/transcriptions', { method: 'POST', headers, body: formData });
          if (resp.ok) {
            const data = await resp.json();
            if (data.text) {
              window.dispatchEvent(new CustomEvent('stt-chunk', { detail: data.text }));
            }
          } else if (!chunkErrorShown) {
            chunkErrorShown = true;
            const msg = await this._readErrMessage(resp);
            if (Alpine.store('toast')) Alpine.store('toast').show('error', msg || Alpine.store('i18n').t('chat_stt_error'));
          }
        } catch (e) {
          console.error('Chunk STT Error', e);
        }
      };

      const checkVAD = () => {
        if (!this.isRecording || !this._wavRecorder) return;

        const volume = this._wavRecorder.getVolume();
        const now = Date.now();

        if (volume > SILENCE_THRESHOLD) {
          if (!isSpeaking) {
            isSpeaking = true;
            silenceStart = null;
          } else {
            silenceStart = null;
          }
        } else {
          if (isSpeaking) {
            if (!silenceStart) {
              silenceStart = now;
            } else if (now - silenceStart > SHORT_PAUSE_MS) {
              isSpeaking = false;
              const chunkBlob = this._wavRecorder.flushChunk();
              if (chunkBlob && chunkBlob.size > 1000) {
                uploadChunk(chunkBlob);
              }
            }
          } else {
            if (silenceStart && now - silenceStart > LONG_PAUSE_MS) {
              this.toggleRecording(); // 长时间无声自动结束录音
              return;
            }
          }
        }
        this._checkVADTimeout = requestAnimationFrame(checkVAD);
      };

      this.isRecording = true;
      checkVAD();

    } catch (e) {
      console.error('Failed to start recording', e);
      Alpine.store('dialog').alert({ message: Alpine.store('i18n').t('chat_stt_mic_error') });
    }
  },

  async _stopRecording() {
    this.isRecording = false;
    if (this._checkVADTimeout) {
      cancelAnimationFrame(this._checkVADTimeout);
      this._checkVADTimeout = null;
    }

    if (!this._wavRecorder) return;

    const globalWavBlob = this._wavRecorder.flushAll();
    this._wavRecorder.close();
    this._wavRecorder = null;

    if (!globalWavBlob || globalWavBlob.size < 1000) {
      return;
    }

    if (Alpine.store('toast')) {
      Alpine.store('toast').show('ok', Alpine.store('i18n').t('chat_stt_global_checking'));
    }

    const formData = new FormData();
    formData.append('file', globalWavBlob, 'global.wav');

    try {
      const headers = authHeaders();
      delete headers['Content-Type'];
      const resp = await fetch('/v1/audio/transcriptions', { method: 'POST', headers, body: formData });
      if (resp.ok) {
        const data = await resp.json();
        if (data.text) {
          window.dispatchEvent(new CustomEvent('stt-final', { detail: data }));
        }
      } else {
        throw new Error((await this._readErrMessage(resp)) || `Status ${resp.status}`);
      }
    } catch (e) {
      console.error('Global STT Failed', e);
      if (Alpine.store('toast')) {
        Alpine.store('toast').show('error', e?.message || Alpine.store('i18n').t('chat_stt_error'));
      }
    }
  },

  // 读取服务端错误文本：优先 JSON 的 message/error 字段，否则取纯文本；失败返回空串。
  async _readErrMessage(resp) {
    try {
      const raw = await resp.text();
      try {
        const j = JSON.parse(raw);
        return j.message || j.error || raw;
      } catch {
        return raw;
      }
    } catch {
      return '';
    }
  },

  // 语音资产门控（kind: 'stt'|'tts'）。返回 true 表示现在即可使用（ready，或 loading——请求会等待加载完成）。
  // 状态机：not_installed→征得同意后 POST install 并轮询进度；downloading→只展示进度；
  // unsupported→说明最低配置；failed→展示原因并允许再次点击重试安装。
  // 语音资产门控（kind: 'stt'|'tts'）。返回 true 表示现在即可使用（ready 或 loading）。
  async _ensureAudioAsset(kind) {
    const audio = Alpine.store('audio')
    if (audio) await audio.refresh()
    const st = audio ? audio[kind] : this.capabilities?.[kind + '_status']
    if (!st) return true
    const t = (k) => Alpine.store('i18n')?.t(k) || k
    const toast = (type, msg, ms, opts) => {
      if (Alpine.store('toast')) Alpine.store('toast').show(type, msg, ms, opts)
    }

    switch (st.state) {
      case 'ready':
      case 'loading':
        return true

      case 'unsupported':
        toast('error', t(kind === 'stt' ? 'audio_stt_unsupported' : 'audio_tts_unsupported'), 8000, { key: `audio-${kind}` })
        return false

      case 'downloading': {
        if (audio) audio.pendingIntent[kind] = true
        const p = audio ? audio.pct(kind) : null
        const pStr = p !== null ? `${p}%` : '…'
        const msg = t(kind === 'stt' ? 'audio_wait_stt' : 'audio_wait_tts').replace('{0}', pStr)
        toast('info', msg, 4000, { key: `audio-${kind}` })
        return false
      }

      case 'not_installed': {
        if (audio && audio.autoInstall) {
          audio.pendingIntent[kind] = true
          audio.install(kind)
          const msg = t(kind === 'stt' ? 'audio_wait_stt' : 'audio_wait_tts').replace('{0}', '…')
          toast('info', msg, 4000, { key: `audio-${kind}` })
          return false
        }
        // auto_install 为 false：弹出页内 modal
        if (audio) {
          const confirmed = await audio.promptInstall(kind)
          if (confirmed) {
            audio.pendingIntent[kind] = true
            audio.install(kind)
            const msg = t(kind === 'stt' ? 'audio_wait_stt' : 'audio_wait_tts').replace('{0}', '…')
            toast('info', msg, 4000, { key: `audio-${kind}` })
          }
        }
        return false
      }

      case 'failed': {
        const errText = st.error || st.detail || t('audio_prep_failed')
        toast('error', errText, 6000, { key: `audio-${kind}` })
        if (audio) {
          audio.pendingIntent[kind] = true
          audio.install(kind)
        }
        return false
      }

      default:
        toast('error', t('chat_stt_not_ready').replace('{0}', st.detail || st.state), 6000, { key: `audio-${kind}` })
        return false
    }
  },

  // 浏览器系统语音兜底：只用本地（localService）中文语音，避免把文本发给云端语音。无可用语音返回 null。
  _pickLocalZhVoice() {
    if (typeof window === 'undefined' || !window.speechSynthesis) return null
    const voices = window.speechSynthesis.getVoices() || []
    return voices.find((v) => v.localService && v.lang && v.lang.startsWith('zh')) || null
  },

  // 决定朗读后端：'server'（MeloTTS）| 'system'（speechSynthesis）| null（不可用，已提示）。
  // 读 Alpine.store('audio')，下载中直接退回系统语音且不弹任何下载 toast。
  async _resolveTTSBackend() {
    const audio = Alpine.store('audio')
    if (audio) await audio.refresh()
    const t = (k) => Alpine.store('i18n')?.t(k) || k
    const toast = (type, msg, ms, opts) => {
      if (Alpine.store('toast')) Alpine.store('toast').show(type, msg, ms, opts)
    }
    const engine = audio ? audio.engine : (this.capabilities?.tts_engine || 'auto')
    const st = audio ? audio.tts : this.capabilities?.tts_status

    const useSystem = () => {
      if (this._pickLocalZhVoice()) return 'system'
      toast('error', t('audio_tts_unavailable'), 6000, { key: 'audio-tts' })
      return null
    }

    if (engine === 'system') return useSystem()
    if (!st) return 'server'
    if (st.state === 'ready' || st.state === 'loading') return 'server'

    // 服务端朗读不可用（B 档内存不足 / 本机 CPU 过慢 / A 档）：系统语音能读就直接读，不弹任何 toast，
    // 原因文案只在状态 chip 展示；仅当系统里也没有本地中文语音时才由 useSystem() 提示（ADR-0110 修订三）。
    // 必须先于 engine==='server' 分支：用户曾选"仅服务端"时，B 档也不该被 unsupported toast 打扰。
    if (st.state === 'unsupported') return useSystem()

    if (engine === 'server') {
      await this._ensureAudioAsset('tts')
      return null
    }

    if (st.state === 'not_installed') {
      if (audio && audio.autoInstall) {
        audio.install('tts')
      }
      return useSystem() // 不弹下载 toast，直接系统语音
    }

    // downloading / unsupported / failed：退回系统语音，零下载 toast
    return useSystem()
  },

  // 用 speechSynthesis 逐句朗读（系统语音兜底）。返回的 Promise 在读完或被取消时结束。
  _speakWithSystemVoice(sentences, isCancelled) {
    const synth = window.speechSynthesis;
    const voice = this._pickLocalZhVoice();
    return new Promise((resolve) => {
      let i = 0;
      const next = () => {
        if (isCancelled() || i >= sentences.length) { synth.cancel(); resolve(); return; }
        const u = new SpeechSynthesisUtterance(sentences[i++]);
        if (voice) { u.voice = voice; u.lang = voice.lang; } else { u.lang = 'zh-CN'; }
        u.onend = next;
        u.onerror = next;
        synth.speak(u);
      };
      next();
    });
  },

  async submit(input) {
    if (!input.trim() && this.attachments.length === 0 || this.isActive) return
    // 本会话仍有后台回合在跑（例如 clearView 把视图摘走后）：服务端同会话同时只允许一个回合。
    if (this.sessionID && this.isSessionRunning(this.sessionID)) return

    // 刚点过中断的上一回合还在收尾：等它结束再发，避免撞 409 session_busy
    if (this.sessionID) await this._awaitAborted(this.sessionID)

    // 新消息发出，清除上次中断状态
    this.lastAbortedInput = null

    // 幂等 runID
    const runID = dedupeRunID(this.sessionID || '', input)

    // 追加用户消息
    this.messages.push({ role: 'user', content: input, toolCalls: [], aborted: false, reasoningContent: '' })
    this._inputHistory.unshift(input)
    if (this._inputHistory.length > 50) this._inputHistory.pop()
    this._historyIdx = -1

    // 新建 run（继续已有会话时沿用会话 ID 作键；新会话用临时键，收到 session 事件后改键）
    const key = this.sessionID || ('tmp_' + crypto.randomUUID())
    this.runs[key] = newRun(key, this.sessionID, this.messages)
    this.viewKey = key
    const run = this.runs[key]
    this.thinkingOpen = true

    const attachmentsPayload = [...this.attachments];
    this.attachments = [];

    const client = new SSEClient({
      url: '/v1/agent/stream',
      body: {
        input,
        session_id: this.sessionID,
        // 仅新会话携带：已存在会话的归属以库内为准（ADR-0097）
        project_id: this.sessionID ? undefined : (Alpine.store('projects').current || undefined),
        run_id: runID,
        attachments: attachmentsPayload,
        model_id: this.selectedModel || undefined,
        reasoning_effort: this.reasoningEffort !== 'auto' ? this.reasoningEffort : undefined,
      },
      onEvent: (type, data) => this._onEvent(run.rid, type, data),
      onError: (err) => this._onError(run.rid, err),
      onComplete: () => this._onComplete(run.rid),
    })
    sseClients.set(run.rid, client)
    client.start()
  },

  // 按 rid 取 run（key 会在 session 事件后改变，SSE 回调只认稳定的 rid）
  _byRid(rid) {
    return Object.values(this.runs).find(r => r.rid === rid) || null
  },

  // 点中断后上一回合在服务端还在收尾，最多等 3s 让其到达终止事件。
  async _awaitAborted(sessionID) {
    const deadline = Date.now() + 3000
    while (Date.now() < deadline) {
      const r = this.runs[sessionID]
      if (!r || !r.aborting || r.finished) return
      await new Promise(res => setTimeout(res, 50))
    }
  },

  // interrupt 中止当前视图所属会话的回合。abort 走 POST /v1/sessions/{id}/abort
  // （连接断开不再中止回合，ADR-0116）；其余 action 沿用 taskID 接口。
  interrupt(action = 'abort') {
    const run = this.viewRun
    if (!run || run.finished) return
    if (action !== 'abort') {
      if (run.taskID) {
        fetch(`/v1/agent/${run.taskID}/interrupt`, {
          method: 'POST',
          headers: authHeaders(),
          body: JSON.stringify({ action }),
        }).catch(e => console.error(e))
      }
      return
    }
    // 记录最后一条用户消息内容，供"恢复编辑"按钮使用
    const lastUser = [...this.messages].reverse().find(m => m.role === 'user')
    this.lastAbortedInput = lastUser ? lastUser.content : null
    if (run.sessionID) {
      this._postAbort(run.sessionID)
    } else {
      run.abortRequested = true // 还没拿到 session 事件，拿到后补发
    }
    // 乐观更新：立即结束可见状态；SSE 保持订阅直到服务端终止事件，期间忽略其余事件
    run.aborting = true
    this._finalizeMessage(run, true)
    run.state = 'COMPLETE'
    this.thinkingOpen = false
    Alpine.store('statusBar').poll()
  },

  _postAbort(sessionID) {
    fetch(`/v1/sessions/${encodeURIComponent(sessionID)}/abort`, {
      method: 'POST',
      headers: authHeaders(),
    }).catch(e => console.error(e))
  },

  // restoreInput 将上次中断的用户消息恢复到输入框，并清除中断状态
  restoreInput() {
    if (!this.lastAbortedInput) return
    window.dispatchEvent(new CustomEvent('restore-input', { detail: this.lastAbortedInput }))
    this.lastAbortedInput = null
  },

  // recallMessage 将指定的消息撤回并填入输入框，同时截断后面的对话
  recallMessage(idx, content) {
    if (this.isActive) return; // 如果正在生成中，不允许撤回
    // 撤回会截断这之后的消息，先给被截断消息里挂着的视图发 teardown 再移除
    // （view 的 iframe DOM 随 Alpine x-for 一起消失，不主动 teardown 会跳过
    // ui/resource-teardown 通知，违反"移除前发通知"的约定）。
    for (const m of this.messages.slice(idx)) {
      for (const v of (m.views || [])) mcpAppsHost.teardown(v.view_id, 'message_removed')
    }
    this.messages.splice(idx);
    window.dispatchEvent(new CustomEvent('restore-input', { detail: content }));
    this.lastAbortedInput = null;
  },

  playingMsgIdx: null,
  _audioPlayer: null,

  async toggleSpeakText(idx, text) {
    if (!text) return;

    if (this.playingMsgIdx === idx && this._audioPlayer) {
      this._audioPlayer.pause();
      this._audioPlayer = null;
      this.playingMsgIdx = null;
      if (window.speechSynthesis) window.speechSynthesis.cancel(); // 系统语音兜底路径的停止
      return;
    }

    if (this._audioPlayer) {
      this._audioPlayer.pause();
      this._audioPlayer = null;
    }

    this.playingMsgIdx = idx;

    // 同步创建一个 Audio 对象并预热（静音/空白），以绕过浏览器的异步长期等待后的自动播放拦截
    const audio = new Audio();
    audio.src = 'data:audio/wav;base64,UklGRigAAABXQVZFZm10IBAAAAABAAEARKwAAIhYAQACABAAZGF0YQQAAAAAAA==';
    audio.play().catch(() => {});

    this._audioPlayer = audio;

    // 预处理文本，去除对于 TTS 不友好的符号和内容，避免产生乱音
    let cleanText = text
      // 去除 Emoji
      .replace(/[\u{1F300}-\u{1F9FF}\u{2600}-\u{26FF}\u{2700}-\u{27BF}\u{1F1E6}-\u{1F1FF}\u{1F600}-\u{1F64F}\u{1F680}-\u{1F6FF}]/gu, '')
      // 去除多行代码块 (TTS 读代码体验很差，直接跳过)
      .replace(/```[\s\S]*?```/g, '')
      // 去除图片语法
      .replace(/!\[.*?\]\(.*?\)/g, '')
      // 提取链接文字
      .replace(/\[([^\]]+)\]\(.*?\)/g, '$1')
      // 移除多余的 Markdown 标记 (粗体、斜体、引用、标题)
      .replace(/[*_~`#>]/g, '')
      // 移除行首的无序列表符
      .replace(/^- /gm, '')
      // 仅规整引号与括号，保留中文标点（，。！？；：、）以保留中文语音合成的自然韵律与停顿
      .replace(/“|”/g, '"')
      .replace(/‘|’/g, "'")
      .replace(/（/g, '(')
      .replace(/）/g, ')')
      .trim();

    // 按句号、感叹号、问号、换行符等标点断句，避免长文本生成过慢
    const regex = /([。？！.?!\n]+)/;
    const parts = cleanText.split(regex);
    const sentences = [];
    for (let i = 0; i < parts.length; i += 2) {
      const sentence = (parts[i] + (parts[i + 1] || '')).trim();
      if (sentence.length > 0) sentences.push(sentence);
    }

    if (sentences.length === 0) {
      this.playingMsgIdx = null;
      this._audioPlayer = null;
      return;
    }

    try {
      // 先决定朗读后端：服务端（MeloTTS）未就绪/不支持时按设置退回系统本地中文语音。
      const backend = await this._resolveTTSBackend();
      if (backend === null || this._audioPlayer !== audio) {
        if (this._audioPlayer === audio) { this._audioPlayer = null; this.playingMsgIdx = null; }
        return;
      }
      if (backend === 'system') {
        await this._speakWithSystemVoice(sentences, () => this._audioPlayer !== audio || this.playingMsgIdx !== idx);
        if (this.playingMsgIdx === idx) { this.playingMsgIdx = null; this._audioPlayer = null; }
        return;
      }

      let isStopped = false;
      
      // 预先清理函数
      const cleanup = () => {
        isStopped = true;
        if (this.playingMsgIdx === idx) {
          this.playingMsgIdx = null;
          this._audioPlayer = null;
        }
      };

      const fetchAudioBlob = async (sentenceText) => {
        const headers = authHeaders();
        headers['Content-Type'] = 'application/json';
        const resp = await fetch('/v1/audio/speech', {
          method: 'POST',
          headers: headers,
          body: JSON.stringify({ input: sentenceText })
        });
        if (!resp.ok) {
          const msg = await this._readErrMessage(resp);
          throw new Error(msg || ('TTS Request Failed: ' + resp.status));
        }
        return resp.blob();
      };

      // 双缓冲异步预取：根据硬件 capabilities 决策是否开启预取（Tier 1+ 开启双缓冲）
      const prefetchLimit = this.capabilities?.tts_prefetch_count ?? 2;
      const shouldPrefetch = prefetchLimit >= 2;
      let nextBlobPromise = null;

      for (let i = 0; i < sentences.length; i++) {
        if (isStopped || this.playingMsgIdx !== idx || this._audioPlayer !== audio) {
          break;
        }

        const sentence = sentences[i];
        let currentBlob;

        if (nextBlobPromise) {
          try {
            currentBlob = await nextBlobPromise;
          } catch (err) {
            console.warn('TTS prefetch failed, retrying on demand:', err);
            currentBlob = await fetchAudioBlob(sentence);
          }
          nextBlobPromise = null;
        } else {
          currentBlob = await fetchAudioBlob(sentence);
        }

        // 若硬件支持且存在下一句，立即触发后台异步拉取
        if (shouldPrefetch && i + 1 < sentences.length && !isStopped) {
          nextBlobPromise = fetchAudioBlob(sentences[i + 1]);
        }

        if (isStopped || this.playingMsgIdx !== idx || this._audioPlayer !== audio) {
          break;
        }

        // 播放当前句子
        const url = URL.createObjectURL(currentBlob);
        audio.src = url;
        
        // 包装 play 在 Promise 中等待结束
        await new Promise((resolve, reject) => {
          audio.onended = resolve;
          audio.onerror = reject;
          audio.play().catch(reject);
        });

        URL.revokeObjectURL(url);
      }
      
      // 播放全部完成
      cleanup();

    } catch (e) {
      console.error('Audio playback failed:', e);
      if (Alpine.store('toast')) {
        Alpine.store('toast').show('error', Alpine.store('i18n').t('chat_tts_error').replace('{0}', e?.message || ''), 6000);
      }
      if (this.playingMsgIdx === idx) {
        this.playingMsgIdx = null;
        this._audioPlayer = null;
      }
    }
  },

  _onEvent(rid, type, data) {
    const run = this._byRid(rid)
    if (!run || run.finished) return
    // 用户已点中断：只等终止事件，其余事件（含迟到的 token）一律忽略
    if (run.aborting && type !== 'complete' && type !== 'error') return
    switch (type) {
      case 'session':
        this._bindSession(run, data.session_id)
        break
      case 'thinking':
        run.state = 'THINKING'
        run.thinkingText += data.content || ''
        break
      case 'token':
        run.state = 'STREAMING'
        run.currentTokens += data.content || ''
        break
      case 'tool_call':
        run.state = 'TOOL_RUNNING'
        // 追加到当前流式消息的 toolCalls
        run.pendingToolCall = { name: data.name || '', input: data.input || {}, output: null }
        break
      case 'tool_result':
        run.state = 'STREAMING'
        if (run.pendingToolCall) {
          run.pendingToolCall.output = data.output || ''
          // 将工具调用记入当前 messages
          if (run.messages.length > 0) {
            const last = run.messages[run.messages.length - 1]
            if (last.role === 'assistant') {
              last.toolCalls.push({ ...run.pendingToolCall })
            }
          } else {
            // 还没有 assistant 消息，先创建占位
            run.messages.push({ role: 'assistant', content: '', reasoningContent: '', toolCalls: [{ ...run.pendingToolCall }], aborted: false })
          }
          run.pendingToolCall = null
        }
        break
      case 'complete':
        if (data && data.session_id) this._bindSession(run, data.session_id)
        if (data && data.duration_ms) {
          const last = run.messages[run.messages.length - 1];
          if (last && last.role === 'assistant') {
            last.taskDuration = data.duration_ms;
          }
        }
        this._onComplete(rid, data)
        break
      case 'error':
        this._onError(rid, data)
        break
      case 'context_warning':
        run.contextWarning = data
        break
      case 'status':
        if (data.type === 'phase') {
          run.phase = data.phase || ''
        } else if (data.type === 'approval_required') {
          // 回合在内核侧阻塞等待裁决；超时按拒绝处理（后端 HITL 网关兜底）
          run.approvals.push({ id: data.id, tool: data.tool || '', input: data.input || '', deadlineNs: data.deadline_ns || 0, busy: false })
        } else if (data.type === 'compacting') {
          run.compacting = true
        } else if (data.type === 'compacted') {
          run.compacting = false
          // 在当前消息列表末尾标记压缩节点，复用 compactionAfter 分隔线渲染
          if (run.messages.length > 0) {
            run.messages[run.messages.length - 1].compactionAfter = true
          }
        } else if (data.type === 'tool_ui') {
          // MCP Apps 视图快照（M8f-2）：挂到当前 assistant 消息，交给
          // mcp_apps.js 在对应 DOM 节点创建时挂载（见 chat.html x-init）。
          const viewRef = {
            view_id: data.view_id,
            server_id: data.server_id,
            resource_uri: data.resource_uri,
            tool_name: data.tool_name,
            tool_input: data.tool_input,
            tool_result: data.tool_result,
            cancelled: !!data.cancelled,
          }
          let last = run.messages[run.messages.length - 1]
          if (!last || last.role !== 'assistant') {
            last = { role: 'assistant', content: '', reasoningContent: '', toolCalls: [], views: [], aborted: false }
            run.messages.push(last)
          }
          if (!last.views) last.views = []
          last.views.push(viewRef)
        }
        if (data && data.agent_id) {
          if (!run.subagents) run.subagents = []
          let sa = run.subagents.find(s => s.id === data.agent_id)
          if (!sa) {
            sa = { id: data.agent_id, parent_id: data.parent_id || '', status: 'running', message: '' }
            run.subagents.push(sa)
          }
          if (data.type === 'subagent_output') {
            // 子智能体正文输出不进入助手回复（R5-01），只在树节点下展示
            sa.output = (sa.output || '') + (data.message || '')
          } else if (data.message) {
            sa.message = data.message
          }
          if (data.type === 'tool_result' || (data.message && data.message.includes('task_done'))) {
            sa.status = 'ok'
          }
        }
        break
    }

    // 从响应体读取 taskID
    if (data && data.task_id && !run.taskID) {
      run.taskID = data.task_id
    }
  },

  // 会话 ID 一经确定（session 事件 / complete 载荷）：新会话 run 由临时键改为 sessionID 键。
  _bindSession(run, sid) {
    if (!sid) return
    if (run.sessionID !== sid) run.sessionID = sid
    if (run.key !== sid) {
      const old = run.key
      this.runs[sid] = run
      delete this.runs[old]
      run.key = sid
      if (this.viewKey === old) this.viewKey = sid
    }
    if (this.viewKey === sid) {
      this.sessionID = sid
      try { localStorage.setItem('polaris_session_id', sid) } catch { /* 私有窗口 */ }
    }
    if (run.abortRequested) {
      run.abortRequested = false
      this._postAbort(sid)
    }
  },

  // run 收尾：释放 SSE；不在视图中的 run 直接移除（历史以库为准，回到该会话时重新加载）。
  _closeRun(run) {
    run.finished = true
    run.aborting = false
    sseClients.get(run.rid)?.stop()
    sseClients.delete(run.rid)
    if (this.viewKey !== run.key && this.runs[run.key] === run) delete this.runs[run.key]
  },

  // 后台会话的完成/出错提示：可点击跳转到该会话。
  _notifyBackground(run, ok) {
    const i18n = Alpine.store('i18n')
    const firstUser = run.messages.find(m => m.role === 'user')
    const title = run.title || (firstUser ? firstUser.content.slice(0, 30) : '') || run.sessionID || ''
    const sid = run.sessionID
    Alpine.store('toast').show(ok ? 'ok' : 'error',
      i18n.t(ok ? 'session_done_toast' : 'session_error_toast').replace('{0}', title), 8000,
      { onClick: sid ? () => { this.loadSession(sid); Alpine.store('nav').navigate('chat') } : null })
  },

  _onComplete(rid, data) {
    const run = this._byRid(rid)
    if (!run || run.finished) return
    const inView = this.viewKey === run.key
    if (run.state === 'ERROR') { this._closeRun(run); return }
    const wasAborting = run.aborting
    if (!wasAborting) {
      this._finalizeMessage(run, false)
      run.phase = ''
      run.approvals = []
      run.subagents = []
      run.state = 'COMPLETE'
      if (inView) this.thinkingOpen = false
      run.thinkingText = ''
    }
    this._closeRun(run)
    if (inView) {
      if (!wasAborting) Alpine.store('statusBar').poll()
    } else if (!wasAborting) {
      this._notifyBackground(run, true)
    }
  },

  // resolveApproval 回复回合内的审批请求。裁决只经 HITL 网关（/v1/approvals），
  // 这里不做任何本地放行判断。
  async resolveApproval(id, action) {
    const item = this.approvals.find(a => a.id === id)
    if (!item || item.busy) return
    item.busy = true
    try {
      const r = await fetch(`/v1/approvals/${encodeURIComponent(id)}/resolve`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ action, comment: '' }),
      })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      this.approvals = this.approvals.filter(a => a.id !== id)
    } catch (err) {
      item.busy = false
      Alpine.store('toast').show('error', `${Alpine.store('i18n').t('chat_approval_failed')}: ${err.message}`)
    }
  },

  _onError(rid, err) {
    const run = this._byRid(rid)
    if (!run || run.finished) return
    const inView = this.viewKey === run.key
    err = err || {}
    if (run.aborting) { this._closeRun(run); return }
    const isAbort = err.code === 'aborted' || err.code === 'interrupted'
    if (err.code === 'session_busy') {
      // 本条消息根本没被服务端接受：撤掉乐观插入的用户消息，提示稍后重试
      const last = run.messages[run.messages.length - 1]
      if (last && last.role === 'user') run.messages.pop()
      err = { ...err, message: Alpine.store('i18n').t('session_busy') }
    } else {
      this._finalizeMessage(run, isAbort)
    }
    run.phase = ''
    run.approvals = []
    run.state = 'ERROR'
    run.errorMsg = err.message || '连接中断'
    this._closeRun(run)
    if (!inView) this._notifyBackground(run, false)
  },

  _finalizeMessage(run, aborted = false) {
    const content = sanitizeContent(run.currentTokens)
    const reasoningContent = sanitizeContent(run.thinkingText)
    if (!content && !aborted && !reasoningContent) return
    const msgs = run.messages
    // 检查是否已有 assistant 消息（tool_result 路径可能提前创建）
    const last = msgs[msgs.length - 1]
    if (last && last.role === 'assistant' && !last.content) {
      last.content = content
      last.reasoningContent = reasoningContent
      last.aborted = aborted
    } else if (content || aborted || reasoningContent) {
      msgs.push({ role: 'assistant', content, reasoningContent, toolCalls: [], aborted })
    }

    if (!aborted && content && this.ttsEnabled && this.viewKey === run.key) {
      this.toggleSpeakText(msgs.length - 1, content.replace(/<[^>]+>/g, '')); // Strip basic HTML for TTS
    }
    run.currentTokens = ''
  },

  // 把当前视图摘下来：不停止任何 SSE——仍在运行的回合继续在后台跑（ADR-0116）。
  clearView() {
    mcpAppsHost.teardownAll('session_changed')
    const run = this.viewRun
    if (run && run.finished && this.runs[run.key] === run) delete this.runs[run.key]
    this.viewKey = null
    this.messages = []   // 新数组；仍在运行的 run 持有旧数组的引用
    this._idle = idleDefaults()
    this.lastAbortedInput = null
  },

  newSession() {
    this._loadSeq++
    this.clearView()
    this.sessionID = null
    this._inputHistory = []
    this._historyIdx = -1
    localStorage.removeItem('polaris_session_id')
  },

  historyUp(currentInput) {
    if (this._inputHistory.length === 0) return currentInput
    this._historyIdx = Math.min(this._historyIdx + 1, this._inputHistory.length - 1)
    return this._inputHistory[this._historyIdx]
  },

  historyDown() {
    if (this._historyIdx <= 0) { this._historyIdx = -1; return '' }
    this._historyIdx--
    return this._inputHistory[this._historyIdx]
  },

  async loadSession(sessionID) {
    const seq = ++this._loadSeq
    this.clearView()
    this.sessionID = sessionID

    // 该会话在本页仍有进行中的回合：直接接回视图（run 持有的 messages 已含用户消息与流式产物）
    const live = this.runs[sessionID]
    if (live && !live.finished) {
      this.viewKey = live.key
      this.messages = live.messages
      return
    }
    if (live) delete this.runs[sessionID] // 已结束的残留，以库为准

    try {
      const r = await fetch(`/v1/sessions/${sessionID}?max_chars=50000`, { headers: authHeaders() })
      if (seq !== this._loadSeq) return // 期间用户又切走了
      if (!r.ok) return
      const d = await r.json()
      if (seq !== this._loadSeq) return
      // 归档项目不接收新会话：恢复其中的会话时不把它设为"当前项目"，否则随后点"新会话"会被拒。
      const proj = d.project_id ? Alpine.store('projects').byID(d.project_id) : null
      if (proj && !proj.archived) Alpine.store('projects').setCurrent(d.project_id)
      this.messages = (d.messages || []).map(m => ({
        role: m.role,
        content: sanitizeContent(m.content),
        reasoningContent: sanitizeContent(m.reasoning_content || ''),
        toolCalls: m.tool_calls || [],
        // views: 历史回放的 MCP Apps 视图（M8f-2），字段名与 SSE tool_ui 载荷对齐，
        // 供 chat.html 用同一套渲染/挂载逻辑处理（见 mcp_apps.js McpAppsHost.mount）。
        views: (m.views || []).map(v => ({
          view_id: v.view_id, server_id: v.server_id, resource_uri: v.resource_uri,
          tool_name: v.tool_name, tool_input: v.tool_input, tool_result: v.tool_result,
          widget_state: v.widget_state,
          cancelled: false,
        })),
        taskDuration: m.task_duration || 0,
        aborted: m.aborted || false,
        compactionAfter: d.compaction_events?.some(e => e.at_message_id === m.id) || false,
      }))
      // 服务端回合仍在后台运行（刷新页面 / 桌面窗口重开）：重新订阅，回放后接实时。
      if (d.running) this._attachRun(sessionID)
    } catch { /* 静默失败，空历史 */ }
  },

  // 订阅服务端已在运行的回合（不 POST，不产生新回合）。回放从 seq=0 开始，
  // 用户消息已随历史加载，故这里只会重建助手侧的流式产物。
  _attachRun(sessionID) {
    this.runs[sessionID] = newRun(sessionID, sessionID, this.messages)
    this.viewKey = sessionID
    const run = this.runs[sessionID]
    run.state = 'THINKING'
    const client = new SSEClient({
      url: '',
      method: 'GET',
      sessionID,
      onEvent: (type, data) => this._onEvent(run.rid, type, data),
      onError: (err) => this._onError(run.rid, err),
      onComplete: () => this._onComplete(run.rid),
      // 回合恰好刚过保留期：没有可订阅的了，按已结束处理，以库为准
      onNoContent: () => {
        this._closeRun(run)
        if (this.viewKey === run.key) {
          delete this.runs[run.key]
          this.viewKey = null
          this.loadSession(sessionID)
        }
      },
    })
    sseClients.set(run.rid, client)
    client.start()
  },
}

// 视图字段 → 当前视图所属 run 的访问器（无 run 时落到 _idle）
Object.defineProperties(chatStore, Object.fromEntries(RUN_FIELDS.map(k => [k, {
  enumerable: true,
  configurable: true,
  get() { const r = this.viewRun; return r ? r[k] : this._idle[k] },
  set(v) { const r = this.viewRun; if (r) r[k] = v; else this._idle[k] = v },
}])))

Alpine.store('chat', chatStore)

