# Specification: Generative UI 集成方案

## 1. 动机与目标
M13 界面层当前主要展示纯文本聊天和 Tool Result 原始 JSON 堆砌。为了提供现代化对话式 AI 体验，系统需要允许 Agent 根据上下文实时输出丰富的用户界面组件（如动态表单、交互式图表、可视化仪表板）。
本方案将定义基于 SSE 的 `ui_component` 事件结构、Agent 层对应的工具契约以及前端 Alpine.js 的沙箱挂载方式。

## 2. API 与数据流

### 2.1 新增 SSE 事件类型 `ui_component`
在 `POST /v1/agent/stream` 的 Server-Sent Events (SSE) 协议中扩展 `ui_component` 类型：
```json
{
  "type": "ui_component",
  "data": {
    "component_id": "chart_12345",
    "component_type": "render_chart",
    "payload": {
      "type": "bar",
      "dataset": [{"x":"Jan","y":10}, {"x":"Feb","y":20}],
      "options": {"title": "Monthly Sales"}
    }
  }
}
```

### 2.2 Agent 工具契约
新增系统级渲染工具组：
1. **`render_chart(type string, dataset []any, options map)`**: 指示客户端渲染 ECharts / Chart.js 图表。
2. **`render_form(schema map, submit_action string)`**: 指示客户端渲染动态表单，用户提交后回调特定的 webhook 或发送特殊事件。
这些工具执行后的返回不仅写入 `events` 表，还会立即作为 `ui_component` 被广播到 M13 的 SSE 长连接通道。

## 3. 前端挂载与沙箱 (Alpine.js)

### 3.1 挂载方式
前端利用 Alpine.js 声明式渲染机制。
当接收到 `ui_component` 事件时，推入 `components` 响应式数组：
```html
<template x-for="comp in components" :key="comp.component_id">
    <div class="dynamic-ui-container">
        <!-- 核心隔离边界：不再盲目信任 raw HTML -->
        <template x-if="comp.component_type === 'render_chart'">
            <div x-data="chartRenderer(comp.payload)"></div>
        </template>
    </div>
</template>
```

### 3.2 安全白名单沙箱方案
为防止跨站脚本攻击 (XSS) 和 UI 劫持：
1. **禁止全量 `x-html` 挂载**：绝不允许后端直接发原生 HTML 代码供前端 `x-html` 注入。所有 Generative UI 必须通过结构化 JSON Payload + 预定义的 Component 模板实现。
2. **白名单校验**：`component_type` 必须经过前端预先注册的组件白名单校验，未知的类型仅作 JSON fallback 渲染。
3. **内容净化**：对于 `render_markdown` 等含文本展示的特殊组件，必须在客户端调用 DOMPurify 净化。后台在存入 `events` 数据库前，也会过一次 `PIIDetector` 与 HTML 标签过滤（仅限允许标签）。

## 4. 后续演进
* 引入响应式事件闭环：允许点击 UI 元素直接触发对 Agent 的指令下发。
* 支持更复杂的复合组件（Dashboards）。
