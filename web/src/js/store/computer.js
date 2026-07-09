document.addEventListener('alpine:init', () => {
    Alpine.store('computer', {
        mode: 'auto_review', // default, auto_review, full_access
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
                    if (data['computer_use_mode']) this.mode = data['computer_use_mode'];
                    if (data['computer_any_app_enabled']) this.anyAppEnabled = data['computer_any_app_enabled'] === 'true';
                    if (data['computer_chrome_enabled']) this.chromeEnabled = data['computer_chrome_enabled'] === 'true';
                }
            } catch (e) {
                console.error("Failed to load computer preferences", e);
            }
        },

        async savePreference(key, value) {
            try {
                await fetch(`/v1/preferences/${key}`, {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ value: value.toString() })
                });
            } catch (e) {
                console.error("Failed to save preference", e);
            }
        },

        setMode(newMode) {
            this.mode = newMode;
            this.savePreference('computer_use_mode', newMode);
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
