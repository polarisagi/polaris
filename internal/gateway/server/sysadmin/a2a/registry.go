// Package a2a 提供 A2A v0.3 服务发现与 Agent Card 注册接口。
// 当前实现为内存注册表，后续可切换到 etcd/consul（GD-14-003）。
package a2a

import (
	"encoding/json"
	"net/http"
	"sync"
)

// AgentEntry A2A Agent 注册条目。
type AgentEntry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Endpoint    string   `json:"endpoint"`
	Skills      []string `json:"skills"`
	Models      []string `json:"models"`
	TrustLevel  int      `json:"trust_level"`
	SandboxTier int      `json:"sandbox_tier"`
	PublicKey   []byte   `json:"public_key,omitempty"`
}

// Registry 内存式 A2A Agent 注册表。
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*AgentEntry
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*AgentEntry)}
}

// HandleRegister POST /a2a/register — Agent 自注册。
func (reg *Registry) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entry AgentEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if entry.Name == "" || entry.Endpoint == "" {
		http.Error(w, "name and endpoint required", http.StatusBadRequest)
		return
	}
	reg.mu.Lock()
	reg.agents[entry.Name] = &entry
	reg.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

// HandleList GET /a2a/agents — 列出已注册 Agent。
func (reg *Registry) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg.mu.RLock()
	list := make([]*AgentEntry, 0, len(reg.agents))
	for _, e := range reg.agents {
		list = append(list, e)
	}
	reg.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// HandleDeregister DELETE /a2a/agents/{name} — 注销 Agent。
func (reg *Registry) HandleDeregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.PathValue("name")
	reg.mu.Lock()
	delete(reg.agents, name)
	reg.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
