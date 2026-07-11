// CLI 客户端命令（init / chat / status / help / version）。
// 全部通过 HTTP 与本地运行的 Polaris 服务通信，不直接访问数据库。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"golang.org/x/term"
)

// ── ANSI 颜色（仅 TTY 生效）─────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiAccent = "\033[38;5;135m" // 紫色
	ansiOk     = "\033[38;5;78m"  // 绿色
	ansiError  = "\033[38;5;203m" // 红色
	ansiWarn   = "\033[38;5;215m" // 橙色
)

var cliTTY = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

func clr(code, s string) string {
	if !cliTTY {
		return s
	}
	return code + s + ansiReset
}

// ── 服务地址 ─────────────────────────────────────────────────────────────────

func cliServerURL() string {
	if u := os.Getenv("POLARIS_SERVER_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:28888"
}

func cliCheckServer() error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(cliServerURL() + "/healthz")
	if err != nil {
		return apperr.Wrap(apperr.CodeNetworkUnavailable, fmt.Sprintf(t("err_server_down"), cliServerURL()), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf(t("err_health"), resp.StatusCode))
	}
	return nil
}

// ── HTTP 工具 ─────────────────────────────────────────────────────────────────

var cliHTTP = &http.Client{Timeout: 15 * time.Second}

func cliGet(path string, out any) error {
	resp, err := cliHTTP.Get(cliServerURL() + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func cliPost(path string, body any, out any) error {
	return cliRequest("POST", path, body, out)
}

func cliPut(path string, body any, out any) error {
	return cliRequest("PUT", path, body, out)
}

func cliRequest(method, path string, body any, out any) error {
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cliServerURL()+path, buf)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cliHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ── 版本 ─────────────────────────────────────────────────────────────────────

func cliVersion() string { return Version }

// ── Help ─────────────────────────────────────────────────────────────────────

func printCLIHelp() {
	fmt.Println()
	fmt.Println(clr(ansiBold+ansiAccent, t("help_title")))
	fmt.Println()
	fmt.Println(clr(ansiBold, t("help_usage")))
	fmt.Println(t("help_desc_p"))
	fmt.Println(t("help_desc_init"))
	fmt.Println(t("help_desc_chat"))
	fmt.Println(t("help_desc_cmd"))
	fmt.Println(t("help_desc_stat"))
	fmt.Println(t("help_desc_ver"))
	fmt.Println(t("help_desc_help"))
	fmt.Println()
	fmt.Println(clr(ansiBold, t("help_repl")))
	fmt.Println(t("help_repl_new"))
	fmt.Println(t("help_repl_sess"))
	fmt.Println(t("help_repl_clr"))
	fmt.Println(t("help_repl_quit"))
	fmt.Println()
	fmt.Println(clr(ansiBold, t("help_env")))
	fmt.Println(t("help_env_url"))
	fmt.Println(t("help_env_cfg"))
	fmt.Println()
}

// ── polaris status ────────────────────────────────────────────────────────────

func runCLIStatus() error {
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	var st map[string]any
	if err := cliGet("/v1/status", &st); err != nil {
		return err
	}
	fmt.Printf("%s  Polaris  %s\n", clr(ansiOk, "●"), cliServerURL())
	if v := st["model_name"]; v != nil && v != "" {
		fmt.Printf("  Model:    %v\n", v)
	}
	if v := st["provider_name"]; v != nil && v != "" {
		fmt.Printf("  Provider: %v\n", v)
	}
	if ti, ok := st["tokens_in"].(float64); ok {
		to, _ := st["tokens_out"].(float64)
		fmt.Printf("  Tokens:   in=%.0f  out=%.0f\n", ti, to)
	}
	if mem, ok := st["mem_mb"].(float64); ok {
		fmt.Printf("  Memory:   %.0f MB\n", mem)
	}
	return nil
}

// ── polaris init ──────────────────────────────────────────────────────────────

func runInit() error { //nolint:gocyclo
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}

	sc := bufio.NewScanner(os.Stdin)

	fmt.Println()
	fmt.Println(clr(ansiBold+ansiAccent, t("init_title")))
	fmt.Println(clr(ansiDim, t("init_subtitle")))

	// ── Step 1: Provider ─────────────────────────────────────────────────────
	fmt.Println(clr(ansiBold, t("init_s1_title")))
	fmt.Println()
	fmt.Println(t("init_s1_opt1"))
	fmt.Println(t("init_s1_opt2"))
	fmt.Println(t("init_s1_opt3"))
	fmt.Println(t("init_s1_opt4"))
	fmt.Println()

	typeMap := map[string]string{"1": "openai_compat", "2": "anthropic", "3": "google_agent_platform", "4": "ollama"}
	namePlaceholder := map[string]string{"openai_compat": "DeepSeek V4", "anthropic": "Claude", "google_agent_platform": "Gemini", "ollama": "Ollama 本地"}
	defaultURLs := map[string]string{"openai_compat": "https://api.openai.com", "ollama": "http://localhost:11434"}

	choice := initPrompt(sc, t("init_p_type"), "1")
	pType := typeMap[choice]
	if pType == "" {
		pType = "openai_compat"
	}
	name := initPrompt(sc, t("init_p_name"), namePlaceholder[pType])
	var baseURL, apiKey string
	if pType == "openai_compat" || pType == "ollama" {
		baseURL = initPrompt(sc, t("init_p_base"), defaultURLs[pType])
	}
	if pType != "ollama" {
		apiKey = initPromptSecret(sc, t("init_p_key"))
	}

	fmt.Print(clr(ansiDim, t("init_p_saving")))
	var provResult map[string]any
	if err := cliPost("/v1/providers", map[string]any{
		"name": name, "type": pType, "base_url": baseURL, "api_key": apiKey, "enabled": true,
	}, &provResult); err != nil {
		fmt.Println()
		return apperr.Wrap(apperr.CodeInternal, t("init_p_fail"), err)
	}
	providerID, _ := provResult["id"].(string)
	fmt.Println(clr(ansiOk, t("init_p_saved")))

	if initPromptBool(sc, t("init_test_q"), true) {
		fmt.Print(clr(ansiDim, t("init_testing")))
		var testRes map[string]any
		_ = cliPost("/v1/providers/"+url.PathEscape(providerID)+"/test", nil, &testRes)
		if ok, _ := testRes["ok"].(bool); ok {
			msg, _ := testRes["message"].(string)
			fmt.Println(clr(ansiOk, fmt.Sprintf(t("init_test_ok"), msg)))
		} else {
			msg, _ := testRes["message"].(string)
			fmt.Println(clr(ansiWarn, fmt.Sprintf(t("init_test_err"), msg)))
			fmt.Println(clr(ansiDim, t("init_test_hint")))
		}
	}
	fmt.Println()

	// ── Step 2: 添加模型 ──────────────────────────────────────────────────────
	fmt.Println(clr(ansiBold, t("init_s2_title")))
	fmt.Println()

	// 追踪已分配角色的 model UUID，合并更新 model-roles
	defaultModelID, reasoningModelID := "", ""

	for {
		modelID := initPrompt(sc, t("init_m_id"), "")
		if modelID == "" {
			break
		}
		fmt.Println(t("init_m_role_t"))
		roleMap := map[string]string{"1": "default", "2": "reasoning", "3": "general"}
		roleChoice := initPrompt(sc, t("init_m_role"), "1")
		role := roleMap[roleChoice]
		if role == "" {
			role = "default"
		}

		var mResult map[string]any
		if err := cliPost("/v1/providers/"+url.PathEscape(providerID)+"/models", map[string]any{ //nolint:nestif
			"model_id": modelID, "name": modelID, "role": role, "enabled": true,
		}, &mResult); err != nil {
			fmt.Fprintln(os.Stderr, clr(ansiError, fmt.Sprintf(t("init_m_fail"), err.Error())))
		} else {
			fmt.Println(clr(ansiOk, t("init_m_ok")))
			// 同步模型角色
			mUUID, _ := mResult["id"].(string)
			if mUUID != "" {
				switch role {
				case "default":
					defaultModelID = mUUID
				case "reasoning":
					reasoningModelID = mUUID
				}
				rolesPayload := map[string]any{}
				if defaultModelID != "" {
					rolesPayload["default_model_id"] = defaultModelID
				}
				if reasoningModelID != "" {
					rolesPayload["reasoning_model_id"] = reasoningModelID
				}
				if len(rolesPayload) > 0 {
					_ = cliPut("/v1/config/model-roles", rolesPayload, nil)
				}
			}
		}
		if !initPromptBool(sc, t("init_m_next"), false) {
			break
		}
	}
	fmt.Println()

	// ── Step 3: 第三方接入（可选）────────────────────────────────────────────────
	fmt.Println(clr(ansiBold, t("init_s3_title")))
	fmt.Println(clr(ansiDim, t("init_s3_desc")))

	if initPromptBool(sc, t("init_c_q"), false) {
		chTypeMap := map[string]string{"1": "telegram", "2": "feishu", "3": "slack", "4": "discord", "5": "webhook"}
		fmt.Println(t("init_c_opts"))
		chChoice := initPrompt(sc, t("init_c_type"), "1")
		chType := chTypeMap[chChoice]
		if chType == "" {
			chType = "telegram"
		}
		chName := initPrompt(sc, t("init_c_name"), chType+" Bot")
		cfg := map[string]any{}
		tokenLabel := map[string]string{"telegram": "Bot Token", "feishu": "App ID", "slack": "Bot Token", "discord": "Bot Token"}
		tokenKey := map[string]string{"telegram": "bot_token", "feishu": "app_id", "slack": "bot_token", "discord": "bot_token"}
		if label, ok := tokenLabel[chType]; ok {
			val := initPromptSecret(sc, "  "+label)
			cfg[tokenKey[chType]] = val
		}
		if err := cliPost("/v1/channels", map[string]any{
			"name": chName, "type": chType, "config": cfg, "enabled": true,
		}, nil); err != nil {
			fmt.Fprintln(os.Stderr, clr(ansiError, fmt.Sprintf(t("init_c_fail"), err.Error())))
		} else {
			fmt.Println(clr(ansiOk, t("init_c_ok")))
		}
	}
	fmt.Println()

	// ── 完成 ──────────────────────────────────────────────────────────────────
	fmt.Println(clr(ansiOk+ansiBold, t("init_done")))
	fmt.Println()
	fmt.Println(t("init_next"))
	fmt.Printf("  %s %s\n", clr(ansiAccent+ansiBold, "polaris chat"), t("init_next_chat"))
	fmt.Printf("  %s %s\n", clr(ansiAccent+ansiBold, "open http://localhost:28888"), t("init_next_web"))
	fmt.Println()
	return nil
}

// ── polaris chat ──────────────────────────────────────────────────────────────

func runChatCmd(args []string) error {
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}

	// 单次问答模式：polaris chat "message"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		msg := strings.Join(args, " ")
		sid, err := cliStreamChat(msg, cliLoadSession())
		if sid != "" {
			cliSaveSession(sid)
		}
		fmt.Println()
		return err
	}

	// 解析 --session <id>
	var sessionID string
	for i, arg := range args {
		if (arg == "--session" || arg == "-s") && i+1 < len(args) {
			sessionID = args[i+1]
		} else if strings.HasPrefix(arg, "--session=") {
			sessionID = strings.TrimPrefix(arg, "--session=")
		}
	}
	if sessionID == "" {
		sessionID = cliLoadSession()
	}

	return runChatREPL(sessionID)
}

func runChatREPL(initialSession string) error { //nolint:gocyclo
	// 非 TTY（管道输入）时省略 banner
	if cliTTY {
		fmt.Println()
		fmt.Println(clr(ansiBold+ansiAccent, t("chat_banner")) + clr(ansiDim, t("chat_quit_hint")))
		fmt.Println(clr(ansiDim, t("chat_nav")))
		if initialSession != "" {
			fmt.Printf("  %s %s\n", clr(ansiDim, t("chat_sess_lbl")), clr(ansiDim, trimID(initialSession)))
		}
		fmt.Println()
	}

	sessionID := initialSession
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 64*1024)

	for {
		if cliTTY {
			fmt.Print(clr(ansiAccent+ansiBold, t("chat_you")))
		}
		if !sc.Scan() {
			break
		}
		input := strings.TrimSpace(sc.Text())
		if input == "" {
			continue
		}

		switch input {
		case "/quit", "/exit", "quit", "exit":
			if cliTTY {
				fmt.Println(clr(ansiDim, t("chat_bye")))
			}
			return nil
		case "/new":
			sessionID = ""
			cliSaveSession("")
			if cliTTY {
				fmt.Println(clr(ansiDim, t("chat_new")))
			}
			continue
		case "/sessions":
			cliPrintSessions()
			continue
		case "/clear":
			if cliTTY {
				fmt.Print("\033[H\033[2J")
			}
			continue
		case "/help":
			if cliTTY {
				fmt.Println(clr(ansiDim, t("chat_h_new")))
				fmt.Println(clr(ansiDim, t("chat_h_sess")))
				fmt.Println(clr(ansiDim, t("chat_h_clr")))
				fmt.Println(clr(ansiDim, t("chat_h_quit")))
				fmt.Println()
			}
			continue
		}

		if cliTTY {
			fmt.Println()
			fmt.Print(clr(ansiAccent, t("chat_agent")))
		}
		newSID, err := cliStreamChat(input, sessionID)
		if cliTTY {
			fmt.Println()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		} else if newSID != "" {
			sessionID = newSID
			cliSaveSession(sessionID)
		}
		if cliTTY {
			fmt.Println()
		}
	}
	return nil
}

// cliStreamChat 通过 SSE 发送消息并流式打印 token。返回 sessionID（可能新建）。
func cliStreamChat(input, sessionID string) (string, error) { //nolint:gocyclo
	body, _ := json.Marshal(map[string]string{
		"input":      input,
		"session_id": sessionID,
	})
	req, err := http.NewRequest("POST", cliServerURL()+"/v1/agent/stream", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeNetworkUnavailable, t("chat_conn_fail"), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return "", apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}

	var newSID string
	sc := bufio.NewScanner(resp.Body)
	var evType string
	thinkingShown := false

	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			evType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch evType {
			case "thinking":
				if !thinkingShown && cliTTY {
					fmt.Print(clr(ansiDim, t("chat_think")))
					thinkingShown = true
				}
			case "token":
				if thinkingShown && cliTTY {
					// 覆盖思考提示：退格 + 清除到行尾
					fmt.Print("\r\033[K" + clr(ansiAccent, t("chat_agent")))
					thinkingShown = false
				}
				var tok struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(data), &tok) == nil {
					fmt.Print(tok.Content)
				}
			case "complete":
				var comp struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal([]byte(data), &comp) == nil {
					newSID = comp.SessionID
				}
			case "error":
				var errEvt struct {
					Message string `json:"message"`
				}
				if json.Unmarshal([]byte(data), &errEvt) == nil {
					return newSID, apperr.New(apperr.CodeInternal, errEvt.Message)
				}
			}
			evType = ""
		}
	}
	return newSID, sc.Err()
}

func cliPrintSessions() {
	var result map[string]any
	if err := cliGet("/v1/sessions", &result); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "  ✗ "+err.Error()))
		return
	}
	sessions, _ := result["sessions"].([]any)
	if len(sessions) == 0 {
		fmt.Println(clr(ansiDim, t("sess_none")))
		fmt.Println()
		return
	}
	fmt.Println(clr(ansiDim, t("sess_recent")))
	for i, s := range sessions {
		if i >= 10 {
			break
		}
		sess, _ := s.(map[string]any)
		id, _ := sess["id"].(string)
		title, _ := sess["title"].(string)
		if title == "" {
			title = t("sess_untitled")
		}
		updated, _ := sess["updated_at"].(string)
		if tVal, err := time.Parse(time.RFC3339, updated); err == nil {
			updated = tVal.In(time.Local).Format("2006-01-02 15:04:05")
		} else if len(updated) > 10 {
			updated = updated[:10]
		}
		fmt.Printf("  %s  %s  %s\n",
			clr(ansiDim, trimID(id)),
			title,
			clr(ansiDim, updated))
	}
	fmt.Println()
}

// ── 会话持久化 ────────────────────────────────────────────────────────────────

func cliSessionFile() string {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".polarisagi/polaris")
	} else if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	return filepath.Join(dir, "last_cli_session")
}

func cliSaveSession(id string) {
	_ = os.WriteFile(cliSessionFile(), []byte(id), 0o600)
}

func cliLoadSession() string {
	b, err := os.ReadFile(cliSessionFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ── 输入辅助 ──────────────────────────────────────────────────────────────────

func initPrompt(sc *bufio.Scanner, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", label, clr(ansiDim, defaultVal))
	} else {
		fmt.Printf("%s: ", label)
	}
	if !sc.Scan() {
		return defaultVal
	}
	v := strings.TrimSpace(sc.Text())
	if v == "" {
		return defaultVal
	}
	return v
}

func initPromptSecret(sc *bufio.Scanner, label string) string {
	fmt.Printf("%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func initPromptBool(sc *bufio.Scanner, label string, def bool) bool {
	yn := "Y/n"
	if !def {
		yn = "y/N"
	}
	fmt.Printf("%s [%s]: ", label, clr(ansiDim, yn))
	if !sc.Scan() {
		return def
	}
	v := strings.ToLower(strings.TrimSpace(sc.Text()))
	if v == "" {
		return def
	}
	return v == "y" || v == "yes"
}

// ── polaris export ────────────────────────────────────────────────────────────

// runExport 将服务端数据备份至本地文件。
//
//	polaris export [outfile]
//
// outfile 默认为 polaris-backup-YYYYMMDD.jsonl。
func runExport(args []string) error {
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	outFile := fmt.Sprintf("polaris-backup-%s.jsonl", time.Now().Format("20060102"))
	if len(args) > 0 {
		outFile = args[0]
	}

	resp, err := cliHTTP.Get(cliServerURL() + "/v1/export/backup")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "export", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("export: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}

	f, err := os.Create(outFile)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "export: create file", err)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "export: write", err)
	}
	fmt.Printf("%s  导出完成: %s  (%d bytes)\n", clr(ansiOk, "✓"), outFile, n)
	return nil
}

// ── polaris import ────────────────────────────────────────────────────────────

// runImport 从本地备份文件恢复数据至服务端。
//
//	polaris import <infile>
func runImport(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "usage: polaris import <backup-file.jsonl>")
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	inFile := args[0]
	f, err := os.Open(inFile)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "import", err)
	}
	defer f.Close()

	req, err := http.NewRequest("POST", cliServerURL()+"/v1/import/backup", f)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "import", err)
	}
	req.Header.Set("Content-Type", "application/jsonl")

	resp, err := cliHTTP.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "import", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("import: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	inserted, _ := result["inserted"].(float64)
	skipped, _ := result["skipped"].(float64)
	fmt.Printf("%s  导入完成: %d 条记录已写入，%d 条跳过（已存在）\n",
		clr(ansiOk, "✓"), int(inserted), int(skipped))
	return nil
}

// ── polaris config ────────────────────────────────────────────────────────────

// runConfigCmd 处理 config 子命令组。
//
//	polaris config budget set <amount_usd>   设置月度预算上限（单位：美元）
//	polaris config budget get                查看当前月度预算
func runConfigCmd(args []string) error {
	if len(args) == 0 {
		fmt.Println("用法: polaris config <子命令>")
		fmt.Println("  budget get              查看月度预算")
		fmt.Println("  budget set <金额USD>    设置月度预算")
		return nil
	}
	if args[0] == "budget" {
		if err := cliCheckServer(); err != nil {
			fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
			return err
		}
		if len(args) < 2 {
			return runBudgetGet()
		}
		switch args[1] {
		case "get":
			return runBudgetGet()
		case "set":
			if len(args) < 3 {
				return apperr.New(apperr.CodeInvalidInput, "用法: polaris config budget set <金额USD>")
			}
			return runBudgetSet(args[2])
		}
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知子命令: polaris config %s", strings.Join(args, " ")))
}

func runBudgetGet() error {
	var result map[string]any
	if err := cliGet("/v1/config/budget", &result); err != nil {
		return err
	}
	monthly, _ := result["monthly_usd"].(float64)
	if monthly == 0 {
		fmt.Println("月度预算: 未设置（无上限）")
	} else {
		fmt.Printf("月度预算: $%.2f / 月\n", monthly)
	}
	return nil
}

func runBudgetSet(amountStr string) error {
	amount := 0.0
	if _, err := fmt.Sscanf(amountStr, "%f", &amount); err != nil || amount < 0 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("无效金额: %q（请输入非负数，如 10.00）", amountStr))
	}
	var result map[string]any
	if err := cliPost("/v1/config/budget", map[string]any{"monthly_usd": amount}, &result); err != nil {
		return err
	}
	if amount == 0 {
		fmt.Println(clr(ansiOk, "✓") + "  月度预算已清除（无上限）")
	} else {
		fmt.Printf("%s  月度预算已设置为 $%.2f / 月\n", clr(ansiOk, "✓"), amount)
	}
	return nil
}

// ── 工具 ──────────────────────────────────────────────────────────────────────

func trimID(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}
