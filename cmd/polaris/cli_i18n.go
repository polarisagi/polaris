package main

import (
	"os"
	"strings"
)

var cliIsZH = true

func init() {
	lang := strings.ToLower(os.Getenv("LANG"))
	if lang != "" && !strings.Contains(lang, "zh") && !strings.Contains(lang, "cn") {
		cliIsZH = false
	}
}

func t(key string) string {
	val, ok := cliDict[key]
	if !ok {
		return key
	}
	if cliIsZH {
		return val[0]
	}
	return val[1]
}

var cliDict = map[string][2]string{
	"err_server_down": {"Polaris 服务未运行（%s）\n  启动方式：另开终端执行 ./bin/polaris 或 make run", "Polaris server not running (%s)\n  To start: open another terminal and run ./bin/polaris or make run"},
	"err_health":      {"服务健康检查返回 HTTP %d", "Health check returned HTTP %d"},
	"help_title":      {"  Polaris AI Agent Harness", "  Polaris AI Agent Harness"},
	"help_usage":      {"Usage:", "Usage:"},
	"help_desc_p":     {"  polaris                  启动 Agent 服务（Web UI + REST API）", "  polaris                  Start Agent Server (Web UI + REST API)"},
	"help_desc_init":  {"  polaris init             交互式初始化向导（配置 LLM 厂商 / 模型 / 第三方接入）", "  polaris init             Interactive setup wizard (LLM providers / models / channels)"},
	"help_desc_chat":  {"  polaris chat             终端对话 REPL（服务需已运行）", "  polaris chat             Terminal chat REPL (server must be running)"},
	"help_desc_cmd":   {"  polaris chat <msg>       单次问答后退出", "  polaris chat <msg>       One-shot chat and exit"},
	"help_desc_stat":  {"  polaris status           查看服务运行状态", "  polaris status           Check server status"},
	"help_desc_ver":   {"  polaris version          显示版本号", "  polaris version          Show version"},
	"help_desc_help":  {"  polaris help             显示此帮助", "  polaris help             Show this help message"},
	"help_repl":       {"Chat REPL 内建命令:", "Chat REPL built-in commands:"},
	"help_repl_new":   {"  /new        开始新对话（清空上下文）", "  /new        Start a new chat (clear context)"},
	"help_repl_sess":  {"  /sessions   列出历史会话", "  /sessions   List chat history"},
	"help_repl_clr":   {"  /clear      清屏", "  /clear      Clear screen"},
	"help_repl_quit":  {"  /quit       退出", "  /quit       Exit REPL"},
	"help_env":        {"Environment:", "Environment:"},
	"help_env_url":    {"  POLARIS_SERVER_URL    API 地址（默认 http://localhost:29999）", "  POLARIS_SERVER_URL    API URL (default http://localhost:29999)"},
	"help_env_cfg":    {"  POLARIS_CONFIG        配置文件路径（默认 configs/defaults.toml）", "  POLARIS_CONFIG        Config file path (default configs/defaults.toml)"},

	"init_title":     {"  ★  Polaris 初始化向导  ", "  ★  Polaris Setup Wizard  "},
	"init_subtitle":  {"  花 2 分钟完成基础配置\n", "  Basic configuration in 2 minutes\n"},
	"init_s1_title":  {"步骤 1/3 · 连接 LLM 服务", "Step 1/3 · Connect LLM Service"},
	"init_s1_opt1":   {"  1) OpenAI 兼容（OpenAI / DeepSeek / 硅基流动等）", "  1) OpenAI Compatible (OpenAI / DeepSeek / SiliconFlow, etc.)"},
	"init_s1_opt2":   {"  2) Anthropic", "  2) Anthropic"},
	"init_s1_opt3":   {"  3) Google", "  3) Google"},
	"init_s1_opt4":   {"  4) Ollama（本地，无需 API Key）", "  4) Ollama (Local, no API Key needed)"},
	"init_p_type":    {"  厂商类型 [1-4]", "  Provider Type [1-4]"},
	"init_p_name":    {"  显示名称", "  Display Name"},
	"init_p_base":    {"  Base URL", "  Base URL"},
	"init_p_key":     {"  API Key", "  API Key"},
	"init_p_saving":  {"  正在保存厂商配置...", "  Saving provider config..."},
	"init_p_saved":   {" ✓", " ✓"},
	"init_p_fail":    {"保存失败: %w", "Failed to save: %w"},
	"init_test_q":    {"  测试连接？", "  Test connection?"},
	"init_testing":   {"  正在测试...", "  Testing..."},
	"init_test_ok":   {" ✓ %s", " ✓ %s"},
	"init_test_err":  {" ✗ %s", " ✗ %s"},
	"init_test_hint": {"  （凭据有误，配置已保存，可在 Web UI 修改）", "  (Invalid credentials, config saved, can edit later in Web UI)"},

	"init_s2_title": {"步骤 2/3 · 添加模型", "Step 2/3 · Add Models"},
	"init_m_id":     {"  模型 ID（如 deepseek-chat，回车跳过）", "  Model ID (e.g. gpt-4o, press Enter to skip)"},
	"init_m_role_t": {"  用于:  1) 对话（默认主力）  2) 推理（复杂任务）  3) 通用", "  Role:  1) Default (chat)  2) Reasoning (complex tasks)  3) General"},
	"init_m_role":   {"  选择 [1-3]", "  Select [1-3]"},
	"init_m_fail":   {"  ✗ 添加失败: %s", "  ✗ Failed to add: %s"},
	"init_m_ok":     {"  ✓ 模型已添加", "  ✓ Model added"},
	"init_m_next":   {"  继续添加？", "  Add another?"},

	"init_s3_title": {"步骤 3/3 · 第三方接入（可选）", "Step 3/3 · Messaging Integrations (Optional)"},
	"init_s3_desc":  {"  接入 Telegram / 飞书等平台。不配置直接回车跳过。\n", "  Connect Telegram / Slack etc. Press Enter to skip.\n"},
	"init_c_q":      {"  现在配置第三方接入？", "  Configure an integration now?"},
	"init_c_skip":   {"  [Enter] 跳过", "  [Enter] to skip"},
	"init_c_type":   {"  接入类型 [1-5]", "  Integration Type [1-5]"},
	"init_c_name":   {"  接入名称", "  Integration Name"},
	"init_c_token":  {"  Bot Token / Webhook Secret", "  Bot Token / Webhook Secret"},
	"init_c_ok":     {"  ✓ 接入已配置", "  ✓ Integration configured"},

	"init_done":      {"✓ 配置完成！", "✓ Setup Complete!"},
	"init_next":      {"  下一步：", "  Next steps:"},
	"init_next_chat": {"      开始终端对话", "      Start terminal chat"},
	"init_next_web":  {"   打开 Web UI", "   Open Web UI"},

	"chat_banner":    {"  ★ POLARIS  ", "  ★ POLARIS  "},
	"chat_quit_hint": {" Ctrl+C / /quit 退出", " Ctrl+C / /quit to exit"},
	"chat_nav":       {"  /new 新建对话  /sessions 历史  /clear 清屏  /help 帮助", "  /new New chat  /sessions History  /clear Clear  /help Help"},
	"chat_sess_lbl":  {"会话:", "Session:"},
	"chat_you":       {"You: ", "You: "},
	"chat_bye":       {"Goodbye!", "Goodbye!"},
	"chat_new":       {"  ↺ 已开始新对话\n", "  ↺ Started new chat\n"},
	"chat_h_new":     {"  /new       新建对话", "  /new       New chat"},
	"chat_h_sess":    {"  /sessions  查看历史会话", "  /sessions  List history"},
	"chat_h_clr":     {"  /clear     清屏", "  /clear     Clear screen"},
	"chat_h_quit":    {"  /quit      退出", "  /quit      Exit"},
	"chat_agent":     {"Agent: ", "Agent: "},
	"chat_conn_fail": {"连接失败: %v", "Connection failed: %v"},
	"chat_think":     {"[思考中...]", "[Thinking...]"},

	"sess_none":     {"  暂无历史会话", "  No chat history"},
	"sess_recent":   {"  最近会话:", "  Recent sessions:"},
	"sess_untitled": {"(无标题)", "(Untitled)"},
}
