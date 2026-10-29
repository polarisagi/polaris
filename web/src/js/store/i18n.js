import Alpine from 'alpinejs'
import { authHeaders, levelGe, sanitizeContent } from '../utils.js'
import { zh, en } from '../i18n.js'
// ══════════════════════════════════════════════════════════════════════════
// store: i18n（中英文切换）
// ══════════════════════════════════════════════════════════════════════════
Alpine.store('i18n', {
  lang: localStorage.getItem('polaris_lang') || 'zh',
  _maps: { zh, en },

  t(key) {
    return this._maps[this.lang]?.[key] ?? this._maps['zh']?.[key] ?? key
  },

  setLang(lang) {
    this.lang = lang
    localStorage.setItem('polaris_lang', lang)
  },
})

