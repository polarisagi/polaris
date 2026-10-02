import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
import { SSEClient, dedupeRunID } from '../sse.js'
import { mcpAppsHost } from '../mcp_apps.js'
import { WavRecorder } from '../audio/wav_recorder.js'
// ══════════════════════════════════════════════════════════════════════════
// store: chat（主对话状态机）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('chat', {
  // States: IDLE | SUBMITTING | THINKING | STREAMING | TOOL_RUNNING | COMPLETE | ERROR
  state: 'IDLE',
  taskID: null,
  sessionID: null,
  messages: [],          // [{role, content, toolCalls, aborted}]
  currentTokens: '',     // 流式追加缓冲
  thinkingText: '',      // 不进 messages[]
  thinkingOpen: true,
  errorMsg: '',
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
  contextWarning: null,  // context_warning SSE 事件携带的数据
  compacting: false,     // status/compacting 事件期间为 true
  phase: '',             // 回合阶段键 perceive/plan/execute/reflect/respond（status/phase 事件，ADR-0098）
  approvals: [],         // 本回合待用户确认的操作（status/approval_required 事件）；回合结束即清空

  get isActive() { return this.state !== 'IDLE' && this.state !== 'COMPLETE' && this.state !== 'ERROR' },

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
        alert(`Failed to upload ${file.name}`);
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
      alert(Alpine.store('i18n').t('chat_stt_mic_error'));
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

  // 决定朗读后端：'server'（Kokoro）| 'system'（speechSynthesis）| null（不可用，已提示）。
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

    // 新消息发出，清除上次中断状态
    this.lastAbortedInput = null

    // 幂等 runID
    const runID = dedupeRunID(this.sessionID || '', input)

    // 追加用户消息
    this.messages.push({ role: 'user', content: input, toolCalls: [], aborted: false, reasoningContent: '' })
    this._inputHistory.unshift(input)
    if (this._inputHistory.length > 50) this._inputHistory.pop()
    this._historyIdx = -1

    this.currentTokens = ''
    this.thinkingText = ''
    this.thinkingOpen = true
    this.errorMsg = ''
    this.phase = ''
    this.approvals = []
    this.state = 'SUBMITTING'

    const attachmentsPayload = [...this.attachments];
    this.attachments = [];

    window._activeSseClient = new SSEClient({
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
      onEvent: (type, data) => this._onEvent(type, data),
      onError: (err) => this._onError(err),
      onComplete: () => this._onComplete(),
    })
    window._activeSseClient.start()
  },

  interrupt(action = 'abort') {
    if (!this.taskID && !window._activeSseClient) return
    // 记录最后一条用户消息内容，供"恢复编辑"按钮使用
    if (action === 'abort') {
      const lastUser = [...this.messages].reverse().find(m => m.role === 'user')
      this.lastAbortedInput = lastUser ? lastUser.content : null
    }
    if (this.taskID) {
      fetch(`/v1/agent/${this.taskID}/interrupt`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ action }),
      }).catch(e => console.error(e))
    }
    if (window._activeSseClient) {
      window._activeSseClient.stop()
      window._activeSseClient = null
    }
    // 乐观更新：立即结束状态
    if (action === 'abort') {
      this._finalizeMessage(true)
      this.state = 'COMPLETE'
      this.thinkingOpen = false
      Alpine.store('statusBar').poll()
    }
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
      // 先决定朗读后端：Kokoro 服务端未就绪/不支持时按设置退回系统本地中文语音。
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

  _onEvent(type, data) {
    switch (type) {
      case 'thinking':
        this.state = 'THINKING'
        this.thinkingText += data.content || ''
        break
      case 'token':
        this.state = 'STREAMING'
        this.currentTokens += data.content || ''
        break
      case 'tool_call':
        this.state = 'TOOL_RUNNING'
        // 追加到当前流式消息的 toolCalls
        this._pendingToolCall = { name: data.name || '', input: data.input || {}, output: null }
        break
      case 'tool_result':
        this.state = 'STREAMING'
        if (this._pendingToolCall) {
          this._pendingToolCall.output = data.output || ''
          // 将工具调用记入当前 messages
          if (this.messages.length > 0) {
            const last = this.messages[this.messages.length - 1]
            if (last.role === 'assistant') {
              last.toolCalls.push({ ...this._pendingToolCall })
            }
          } else {
            // 还没有 assistant 消息，先创建占位
            this.messages.push({ role: 'assistant', content: '', reasoningContent: '', toolCalls: [{ ...this._pendingToolCall }], aborted: false })
          }
          this._pendingToolCall = null
        }
        break
      case 'complete':
        if (data && data.session_id) {
          this.sessionID = data.session_id
          localStorage.setItem('polaris_session_id', data.session_id)
        }
        if (data && data.duration_ms) {
          const last = this.messages[this.messages.length - 1];
          if (last && last.role === 'assistant') {
            last.taskDuration = data.duration_ms;
          }
        }
        this._onComplete()
        break
      case 'error':
        this._onError(data)
        break
      case 'context_warning':
        this.contextWarning = data
        break
      case 'status':
        if (data.type === 'phase') {
          this.phase = data.phase || ''
        } else if (data.type === 'approval_required') {
          // 回合在内核侧阻塞等待裁决；超时按拒绝处理（后端 HITL 网关兜底）
          this.approvals.push({ id: data.id, tool: data.tool || '', input: data.input || '', deadlineNs: data.deadline_ns || 0, busy: false })
        } else if (data.type === 'compacting') {
          this.compacting = true
        } else if (data.type === 'compacted') {
          this.compacting = false
          // 在当前消息列表末尾标记压缩节点，复用 compactionAfter 分隔线渲染
          if (this.messages.length > 0) {
            this.messages[this.messages.length - 1].compactionAfter = true
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
          let last = this.messages[this.messages.length - 1]
          if (!last || last.role !== 'assistant') {
            last = { role: 'assistant', content: '', reasoningContent: '', toolCalls: [], views: [], aborted: false }
            this.messages.push(last)
          }
          if (!last.views) last.views = []
          last.views.push(viewRef)
        }
        break
    }

    // 从响应体读取 taskID
    if (data && data.task_id && !this.taskID) {
      this.taskID = data.task_id
    }
  },

  _onComplete() {
    if (this.state === 'ERROR') {
      window._activeSseClient = null
      return
    }
    this._finalizeMessage(false)
    this.phase = ''
    this.approvals = []
    this.state = 'COMPLETE'
    this.thinkingOpen = false
    this.thinkingText = ''
    window._activeSseClient = null
    Alpine.store('statusBar').poll()
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

  _onError(err) {
    const isAbort = err.code === 'aborted' || err.code === 'interrupted'
    this._finalizeMessage(isAbort)
    this.phase = ''
    this.approvals = []
    this.state = 'ERROR'
    this.errorMsg = err.message || '连接中断'
    window._activeSseClient?.stop()
    window._activeSseClient = null
  },

  _finalizeMessage(aborted = false) {
    const content = sanitizeContent(this.currentTokens)
    const reasoningContent = sanitizeContent(this.thinkingText)
    if (!content && !aborted && !reasoningContent) return
    // 检查是否已有 assistant 消息（tool_result 路径可能提前创建）
    const last = this.messages[this.messages.length - 1]
    if (last && last.role === 'assistant' && !last.content) {
      last.content = content
      last.reasoningContent = reasoningContent
      last.aborted = aborted
    } else if (content || aborted || reasoningContent) {
      this.messages.push({ role: 'assistant', content, reasoningContent, toolCalls: [], aborted })
    }
    
    if (!aborted && content && this.ttsEnabled) {
      this.toggleSpeakText(this.messages.length - 1, content.replace(/<[^>]+>/g, '')); // Strip basic HTML for TTS
    }
    this.currentTokens = ''
  },

  clearView() {
    mcpAppsHost.teardownAll('session_changed')
    this.messages = []
    this.currentTokens = ''
    this.thinkingText = ''
    this.errorMsg = ''
    this.contextWarning = null
    this.compacting = false
    this.state = 'IDLE'
    this.lastAbortedInput = null
    window._activeSseClient?.stop()
    window._activeSseClient = null
    this.taskID = null
  },

  newSession() {
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
    this.clearView()
    this.sessionID = sessionID
    try {
      const r = await fetch(`/v1/sessions/${sessionID}?max_chars=50000`, { headers: authHeaders() })
      if (!r.ok) return
      const d = await r.json()
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
    } catch { /* 静默失败，空历史 */ }
  },
})

