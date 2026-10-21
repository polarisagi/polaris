import { authHeaders } from '../utils.js';

document.addEventListener('alpine:init', () => {
    Alpine.store('computer', {
        mode: 'auto_review', // 权限模式：default | auto_review | full_access（偏好键 permission_mode）
        rules: [], // 各项目已记住的“以后不再询问”授权（ADR-0117）
        anyAppEnabled: false,
        chromeEnabled: false,

        async init() {
            await this.loadPreferences();
        },

        async loadPreferences() {
            try {
                const res = await fetch('/v1/preferences');
                if (res.ok) {
                    const data = await res.json();
                    const m = data['permission_mode'] || data['computer_use_mode'];
                    if (m) this.mode = m;
                    if (data['computer_any_app_enabled']) this.anyAppEnabled = data['computer_any_app_enabled'] === 'true';
                    if (data['computer_chrome_enabled']) this.chromeEnabled = data['computer_chrome_enabled'] === 'true';
                }
            } catch (e) {
                console.error("Failed to load computer preferences", e);
            }
        },

        async savePreference(key, value) {
            try {
                const res = await fetch(`/v1/preferences/${key}`, {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ value: value.toString() })
                });
                return res.ok;
            } catch (e) {
                console.error("Failed to save preference", e);
                return false;
            }
        },

        async setMode(newMode) {
            const prev = this.mode;
            this.mode = newMode;
            // 服务端可能拒绝（如 hardened 档不允许 full_access）：回滚并提示，不能静默。
            if (!(await this.savePreference('permission_mode', newMode))) {
                this.mode = prev;
                const msg = Alpine.store('i18n')?.t('computer_mode_set_failed') || 'Failed to save permission mode';
                Alpine.store('toast').show('error', msg);
            }
        },

        async loadRules() {
            try {
                const r = await fetch('/v1/approval-rules', { headers: authHeaders() });
                if (r.ok) this.rules = (await r.json()).rules || [];
            } catch (e) {
                console.error('Failed to load approval rules', e);
            }
        },

        async deleteRule(id) {
            try {
                const r = await fetch(`/v1/approval-rules/${encodeURIComponent(id)}`, { method: 'DELETE', headers: authHeaders() });
                if (!r.ok && r.status !== 204) throw new Error(`HTTP ${r.status}`);
                this.rules = this.rules.filter(x => x.id !== id);
                if (Alpine.store('projects')?.load) Alpine.store('projects').load();
            } catch (e) {
                Alpine.store('toast').show('error', e.message);
            }
        },

        projectName(id) {
            const p = (Alpine.store('projects')?.list || []).find(x => x.id === id);
            return p ? p.name : id;
        },

        toggleAnyApp() {
            this.anyAppEnabled = !this.anyAppEnabled;
            this.savePreference('computer_any_app_enabled', this.anyAppEnabled);
        },

        toggleChrome() {
            this.chromeEnabled = !this.chromeEnabled;
            this.savePreference('computer_chrome_enabled', this.chromeEnabled);
        }
    });
});
