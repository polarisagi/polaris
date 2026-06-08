import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
import { SLASH_COMMANDS } from './components_slash.js'
// ── Alpine 组件：聊天输入框 ───────────────────────────────────────────────
Alpine.data('chatInput', () => ({
  input: '',
  showSlash: false,
  slashFocus: 0,
  slashItems: [],
  rows: 1,

  init() {
    this.$watch('input', v => {
      this.rows = Math.min(8, (v.match(/\n/g) || []).length + 1)
      if (v.startsWith('/')) {
        this.slashItems = SLASH_COMMANDS.filter(c => c.cmd.startsWith(v.split(' ')[0]))
        this.showSlash = this.slashItems.length > 0
        this.slashFocus = 0
      } else {
        this.showSlash = false
      }
    })
    window.addEventListener('stt-result', (e) => {
      this.input += (this.input ? ' ' : '') + e.detail;
    });
    // 恢复编辑：将中断的用户消息填回输入框并聚焦
    window.addEventListener('restore-input', (e) => {
      this.input = e.detail || '';
      this.rows = Math.min(8, (this.input.match(/\n/g) || []).length + 1);
      this.$nextTick(() => {
        const ta = document.getElementById('input');
        if (ta) {
          ta.style.height = 'auto';
          ta.style.height = Math.min(ta.scrollHeight, 192) + 'px';
          ta.focus();
          ta.setSelectionRange(ta.value.length, ta.value.length);
        }
      });
    });
  },

  processFiles(files) {
    for (const file of files) {
      Alpine.store('chat').uploadFile(file);
    }
  },

  handlePaste(e) {
    if (e.clipboardData && e.clipboardData.files && e.clipboardData.files.length > 0) {
      e.preventDefault();
      this.processFiles(e.clipboardData.files);
    }
  },

  handleDrop(e) {
    e.preventDefault();
    if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length > 0) {
      this.processFiles(e.dataTransfer.files);
    }
  },

  handleFileSelect(e) {
    if (e.target.files && e.target.files.length > 0) {
      this.processFiles(e.target.files);
    }
    e.target.value = ''; // reset so the same file can be selected again
  },

  onKeydown(e) {
    // 斜杠菜单导航
    if (this.showSlash) {
      if (e.key === 'ArrowDown') { e.preventDefault(); this.slashFocus = (this.slashFocus + 1) % this.slashItems.length; return }
      if (e.key === 'ArrowUp')   { e.preventDefault(); this.slashFocus = (this.slashFocus - 1 + this.slashItems.length) % this.slashItems.length; return }
      if (e.key === 'Tab' || e.key === 'Enter') {
        e.preventDefault()
        this.selectSlash(this.slashItems[this.slashFocus])
        return
      }
      if (e.key === 'Escape') { this.showSlash = false; return }
    }

    // 提交
    if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey) {
      e.preventDefault()
      this.submit()
      return
    }
    // Ctrl+C → abort
    if (e.key === 'c' && e.ctrlKey && Alpine.store('chat').isActive) {
      e.preventDefault()
      Alpine.store('chat').interrupt('abort')
      return
    }
    // Ctrl+K → clear
    if (e.key === 'k' && e.ctrlKey) {
      e.preventDefault()
      Alpine.store('chat').clearView()
      return
    }
    // 历史导航
    if (e.key === 'ArrowUp' && !this.input) {
      e.preventDefault()
      this.input = Alpine.store('chat').historyUp(this.input)
      return
    }
    if (e.key === 'ArrowDown' && Alpine.store('chat')._historyIdx >= 0) {
      e.preventDefault()
      this.input = Alpine.store('chat').historyDown()
      return
    }
  },

  selectSlash(item) {
    const nav = Alpine.store('nav')
    switch (item.cmd) {
      case '/sessions': nav.navigate('sessions');     Alpine.store('sessions').load(); break
      case '/skills':   nav.navigate('skills');        Alpine.store('skills').load(); break
      case '/status':   nav.navigate('monitor');                                     break
      case '/clear':    Alpine.store('chat').clearView();                          break
      case '/compact':  this.sendCompact();                                        break
      default: Alpine.store('toast').show('ok', `命令 ${item.cmd} 暂未实现`)
    }
    this.input = ''
    this.showSlash = false
  },

  async sendCompact() {
    Alpine.store('toast').show('ok', '正在触发上下文压缩...')
    // TODO: POST /v1/agent/compact
  },

  submit() {
    const v = this.input.trim()
    const hasAttachments = Alpine.store('chat').attachments.length > 0;
    if (!v && !hasAttachments || Alpine.store('chat').isActive) return
    Alpine.store('chat').submit(v)
    this.input = ''
    this.rows = 1
  },
}))

