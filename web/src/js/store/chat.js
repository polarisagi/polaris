import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
import { SSEClient, dedupeRunID } from '../sse.js'
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
  ttsEnabled: false,
  isRecording: false,
  _mediaRecorder: null,
  _audioChunks: [],
  lastAbortedInput: null,  // 上次被中断的用户输入内容，用于恢复编辑按钮

  get isActive() { return this.state !== 'IDLE' && this.state !== 'COMPLETE' && this.state !== 'ERROR' },

  toggleTTS() {
    this.ttsEnabled = !this.ttsEnabled;
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
      if (this._mediaRecorder && this._mediaRecorder.state !== 'inactive') {
        this._mediaRecorder.stop();
      }
      return;
    }

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });

      // 按优先级探测浏览器支持的 mimeType：
      // webm（Chrome/Firefox） → mp4（iOS Safari 14.3+） → ogg（Firefox） → 系统默认
      const preferredTypes = ['audio/webm', 'audio/mp4', 'audio/ogg'];
      let mimeType = '';
      for (const t of preferredTypes) {
        if (typeof MediaRecorder !== 'undefined' && MediaRecorder.isTypeSupported(t)) {
          mimeType = t;
          break;
        }
      }

      const recorderOpts = mimeType ? { mimeType } : {};
      this._mediaRecorder = new MediaRecorder(stream, recorderOpts);
      this._audioChunks = [];

      this._mediaRecorder.ondataavailable = (e) => {
        if (e.data.size > 0) {
          this._audioChunks.push(e.data);
        }
      };

      this._mediaRecorder.onstop = async () => {
        this.isRecording = false;
        stream.getTracks().forEach(track => track.stop());

        // 使用实际录制时协商的 mimeType（Safari 可能是 audio/mp4 而非 audio/webm）
        const actualMime = this._mediaRecorder.mimeType || mimeType || 'audio/webm';
        const ext = actualMime.includes('mp4') ? 'mp4' : actualMime.includes('ogg') ? 'ogg' : 'webm';
        const audioBlob = new Blob(this._audioChunks, { type: actualMime });
        this._audioChunks = [];
        
        if (Alpine.store('toast')) {
          Alpine.store('toast').show('ok', '正在识别语音...');
        }

        const formData = new FormData();
        formData.append('file', audioBlob, `recording.${ext}`);
        
        try {
          const headers = authHeaders();
          delete headers['Content-Type'];

          const resp = await fetch('/v1/audio/transcriptions', {
            method: 'POST',
            headers: headers,
            body: formData
          });

          if (resp.ok) {
            const data = await resp.json();
            if (data.text) {
              window.dispatchEvent(new CustomEvent('stt-result', { detail: data.text }));
            }
          } else {
            throw new Error(`Status ${resp.status}`);
          }
        } catch (e) {
          console.error("STT Failed", e);
          if (Alpine.store('toast')) {
            Alpine.store('toast').show('error', '语音识别失败');
          }
        }
      };

      this._mediaRecorder.start();
      this.isRecording = true;

    } catch (e) {
      console.error("Failed to start recording", e);
      alert('无法访问麦克风，请检查浏览器权限');
    }
  },

  async submit(input) {
    if (!input.trim() && this.attachments.length === 0 || this.isActive) return

    // 新消息发出，清除上次中断状态
    this.lastAbortedInput = null

    // 幂等 runID
    const runID = dedupeRunID(this.sessionID || '', input)

    // 追加用户消息
    this.messages.push({ role: 'user', content: input, toolCalls: [], aborted: false })
    this._inputHistory.unshift(input)
    if (this._inputHistory.length > 50) this._inputHistory.pop()
    this._historyIdx = -1

    this.currentTokens = ''
    this.thinkingText = ''
    this.thinkingOpen = true
    this.errorMsg = ''
    this.state = 'SUBMITTING'

    const attachmentsPayload = [...this.attachments];
    this.attachments = [];

    window._activeSseClient = new SSEClient({
      url: '/v1/agent/stream',
      body: {
        input,
        session_id: this.sessionID,
        run_id: runID,
        attachments: attachmentsPayload,
      },
      onEvent: (type, data) => this._onEvent(type, data),
      onError: (err) => this._onError(err),
      onComplete: () => this._onComplete(),
    })
    window._activeSseClient.start()
  },

  interrupt(action = 'abort') {
    if (!this.taskID) return
    // 记录最后一条用户消息内容，供"恢复编辑"按钮使用
    if (action === 'abort') {
      const lastUser = [...this.messages].reverse().find(m => m.role === 'user')
      this.lastAbortedInput = lastUser ? lastUser.content : null
    }
    fetch(`/v1/agent/${this.taskID}/interrupt`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ action }),
    })
    // 乐观更新：如果是 abort，立即标记中断徽章
    if (action === 'abort' && this.currentTokens) {
      this._finalizeMessage(true)
    }
  },

  // restoreInput 将上次中断的用户消息恢复到输入框，并清除中断状态
  restoreInput() {
    if (!this.lastAbortedInput) return
    window.dispatchEvent(new CustomEvent('restore-input', { detail: this.lastAbortedInput }))
    this.lastAbortedInput = null
  },

  speakText(text) {
    if (!this.ttsEnabled || !window.speechSynthesis || !text) return;
    window.speechSynthesis.cancel(); // 中断上一条未播完的语音
    const utterance = new SpeechSynthesisUtterance(text);
    // 优先使用中文语音，回退到系统默认
    const voices = window.speechSynthesis.getVoices();
    const zhVoice = voices.find(v => v.lang.startsWith('zh'));
    if (zhVoice) utterance.voice = zhVoice;
    utterance.lang = zhVoice ? zhVoice.lang : 'zh-CN';
    window.speechSynthesis.speak(utterance);
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
            this.messages.push({ role: 'assistant', content: '', toolCalls: [{ ...this._pendingToolCall }], aborted: false })
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
    this.state = 'COMPLETE'
    this.thinkingOpen = false
    window._activeSseClient = null
    Alpine.store('statusBar').poll()
    // 回复结束后自动朗读（TTS 开关打开时）
    if (this.ttsEnabled && this.currentTokens) {
      this.speakText(this.currentTokens)
    }
  },

  _onError(err) {
    const isAbort = err.code === 'aborted' || err.code === 'interrupted'
    this._finalizeMessage(isAbort)
    this.state = 'ERROR'
    this.errorMsg = err.message || '连接中断'
    window._activeSseClient?.stop()
    window._activeSseClient = null
  },

  _finalizeMessage(aborted = false) {
    const content = sanitizeContent(this.currentTokens)
    if (!content && !aborted) return
    // 检查是否已有 assistant 消息（tool_result 路径可能提前创建）
    const last = this.messages[this.messages.length - 1]
    if (last && last.role === 'assistant' && !last.content) {
      last.content = content
      last.aborted = aborted
    } else if (content || aborted) {
      this.messages.push({ role: 'assistant', content, toolCalls: [], aborted })
    }
    
    if (!aborted && content) {
      this.speakText(content.replace(/<[^>]+>/g, '')); // Strip basic HTML for TTS
    }
    this.currentTokens = ''
  },

  clearView() {
    this.messages = []
    this.currentTokens = ''
    this.thinkingText = ''
    this.errorMsg = ''
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
      this.messages = (d.messages || []).map(m => ({
        role: m.role,
        content: sanitizeContent(m.content),
        toolCalls: m.tool_calls || [],
        taskDuration: m.task_duration || 0,
        aborted: m.aborted || false,
        compactionAfter: d.compaction_events?.some(e => e.at_message_id === m.id) || false,
      }))
    } catch { /* 静默失败，空历史 */ }
  },
})

