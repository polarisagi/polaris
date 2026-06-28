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
  selectedModel: '',
  reasoningEffort: 'auto',
  contextWarning: null,  // context_warning SSE 事件携带的数据
  compacting: false,     // status/compacting 事件期间为 true

  get isActive() { return this.state !== 'IDLE' && this.state !== 'COMPLETE' && this.state !== 'ERROR' },

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
      if (this._globalRecorder && this._globalRecorder.state !== 'inactive') {
        this._globalRecorder.stop();
      }
      return;
    }

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      window.dispatchEvent(new CustomEvent('stt-start'));

      const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
      const audioContext = new AudioContextCtor();
      const analyser = audioContext.createAnalyser();
      const source = audioContext.createMediaStreamSource(stream);
      source.connect(analyser);

      analyser.fftSize = 512;
      const bufferLength = analyser.frequencyBinCount;
      const dataArray = new Uint8Array(bufferLength);

      let silenceStart = null;
      const SILENCE_THRESHOLD = 5;
      const SHORT_PAUSE_MS = 500;
      const LONG_PAUSE_MS = 2500;

      this._globalChunks = [];
      this._currentChunkChunks = [];

      const preferredTypes = ['audio/webm', 'audio/mp4', 'audio/ogg'];
      let mimeType = '';
      for (const t of preferredTypes) {
        if (typeof MediaRecorder !== 'undefined' && MediaRecorder.isTypeSupported(t)) {
          mimeType = t;
          break;
        }
      }
      const recorderOpts = mimeType ? { mimeType } : {};

      // Global Recorder
      this._globalRecorder = new MediaRecorder(stream, recorderOpts);
      this._globalRecorder.ondataavailable = (e) => {
        if (e.data.size > 0) this._globalChunks.push(e.data);
      };

      // Chunk Recorder
      this._chunkRecorder = new MediaRecorder(stream, recorderOpts);
      this._chunkRecorder.ondataavailable = (e) => {
        if (e.data.size > 0) this._currentChunkChunks.push(e.data);
      };

      let isSpeaking = false;
      let checkVADTimeout;

      const uploadChunk = async (blob, ext) => {
        const formData = new FormData();
        formData.append('file', blob, `chunk.${ext}`);
        try {
          const headers = authHeaders();
          delete headers['Content-Type'];
          const resp = await fetch('/v1/audio/transcriptions', { method: 'POST', headers, body: formData });
          if (resp.ok) {
            const data = await resp.json();
            if (data.text) {
              window.dispatchEvent(new CustomEvent('stt-chunk', { detail: data.text }));
            }
          }
        } catch (e) {
          console.error("Chunk STT Error", e);
        }
      };

      this._chunkRecorder.onstop = () => {
        if (this._currentChunkChunks.length === 0) return;
        const actualMime = this._chunkRecorder.mimeType || mimeType || 'audio/webm';
        const ext = actualMime.includes('mp4') ? 'mp4' : actualMime.includes('ogg') ? 'ogg' : 'webm';
        const audioBlob = new Blob(this._currentChunkChunks, { type: actualMime });
        this._currentChunkChunks = [];
        uploadChunk(audioBlob, ext);
      };

      const checkVAD = () => {
        if (!this.isRecording) return;

        analyser.getByteFrequencyData(dataArray);
        let sum = 0;
        for (let i = 0; i < bufferLength; i++) { sum += dataArray[i]; }
        const average = sum / bufferLength;

        const now = Date.now();
        if (average > SILENCE_THRESHOLD) {
          if (!isSpeaking) {
            isSpeaking = true;
            silenceStart = null;
            if (this._chunkRecorder.state === 'inactive') {
              this._currentChunkChunks = [];
              this._chunkRecorder.start();
            }
          } else {
            silenceStart = null;
          }
        } else {
          if (isSpeaking) {
            if (!silenceStart) silenceStart = now;
            else if (now - silenceStart > SHORT_PAUSE_MS) {
              isSpeaking = false;
              if (this._chunkRecorder.state !== 'inactive') {
                this._chunkRecorder.stop();
              }
            }
          } else {
            if (silenceStart && now - silenceStart > LONG_PAUSE_MS) {
              this.toggleRecording(); // Auto stop on long silence
              return;
            }
          }
        }
        checkVADTimeout = requestAnimationFrame(checkVAD);
      };

      this._globalRecorder.onstop = async () => {
        this.isRecording = false;
        cancelAnimationFrame(checkVADTimeout);
        if (this._chunkRecorder.state !== 'inactive') {
          this._chunkRecorder.stop();
        }
        stream.getTracks().forEach(track => track.stop());
        audioContext.close();

        const actualMime = this._globalRecorder.mimeType || mimeType || 'audio/webm';
        const ext = actualMime.includes('mp4') ? 'mp4' : actualMime.includes('ogg') ? 'ogg' : 'webm';
        const audioBlob = new Blob(this._globalChunks, { type: actualMime });
        this._globalChunks = [];

        if (Alpine.store('toast')) {
          Alpine.store('toast').show('ok', Alpine.store('i18n').t('chat_stt_global_checking'));
        }

        const formData = new FormData();
        formData.append('file', audioBlob, `global.${ext}`);

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
            throw new Error(`Status ${resp.status}`);
          }
        } catch (e) {
          console.error("Global STT Failed", e);
          if (Alpine.store('toast')) Alpine.store('toast').show('error', Alpine.store('i18n').t('chat_stt_error'));
        }
      };

      this._globalRecorder.start();
      this.isRecording = true;
      checkVAD();

    } catch (e) {
      console.error("Failed to start recording", e);
      alert(Alpine.store('i18n').t('chat_stt_mic_error'));
    }
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
      // 将中文标点统一替换为英文标点，帮助海外核心的 TTS 模型正确识别停顿
      .replace(/，/g, ', ')
      .replace(/。/g, '. ')
      .replace(/！/g, '! ')
      .replace(/？/g, '? ')
      .replace(/：/g, ': ')
      .replace(/；/g, '; ')
      .replace(/“|”/g, '"')
      .replace(/‘|’/g, "'")
      .replace(/（/g, ' ( ')
      .replace(/）/g, ' ) ')
      .replace(/、/g, ', ')
      .trim();

    // 按句号、感叹号、问号、换行符进行断句，避免长文本生成过慢
    const regex = /([。？！.?!]|\n+)/;
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
      let isStopped = false;
      
      // 预先清理函数
      const cleanup = () => {
        isStopped = true;
        if (this.playingMsgIdx === idx) {
          this.playingMsgIdx = null;
          this._audioPlayer = null;
        }
      };

      for (let i = 0; i < sentences.length; i++) {
        // 如果中途被切断或按了停止按钮
        if (isStopped || this.playingMsgIdx !== idx || this._audioPlayer !== audio) {
          break;
        }

        const sentence = sentences[i];
        
        // 抓取当前句子的音频
        const resp = await fetch('/v1/audio/speech', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ input: sentence })
        });
        
        if (!resp.ok) throw new Error('TTS Request Failed');
        
        const blob = await resp.blob();
        const url = URL.createObjectURL(blob);

        if (isStopped || this.playingMsgIdx !== idx || this._audioPlayer !== audio) {
          URL.revokeObjectURL(url);
          break;
        }

        // 播放当前句子
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
        if (data.type === 'compacting') {
          this.compacting = true
        } else if (data.type === 'compacted') {
          this.compacting = false
          // 在当前消息列表末尾标记压缩节点，复用 compactionAfter 分隔线渲染
          if (this.messages.length > 0) {
            this.messages[this.messages.length - 1].compactionAfter = true
          }
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
    this.state = 'COMPLETE'
    this.thinkingOpen = false
    this.thinkingText = ''
    window._activeSseClient = null
    Alpine.store('statusBar').poll()
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
      this.messages = (d.messages || []).map(m => ({
        role: m.role,
        content: sanitizeContent(m.content),
        reasoningContent: sanitizeContent(m.reasoning_content || ''),
        toolCalls: m.tool_calls || [],
        taskDuration: m.task_duration || 0,
        aborted: m.aborted || false,
        compactionAfter: d.compaction_events?.some(e => e.at_message_id === m.id) || false,
      }))
    } catch { /* 静默失败，空历史 */ }
  },
})

