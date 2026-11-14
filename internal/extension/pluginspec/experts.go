package pluginspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const RuleExpertsShape = "polaris.experts.shape"

type expertParseState struct {
	known       map[string]bool
	seen        map[string]bool
	seenEntries map[string]bool
}

// Expert 只收录显式声明，避免把插件内所有 Agent 自动暴露为产品角色。
type Expert struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	EntryAgent   string              `json:"entryAgent"`
	DisplayName  map[string]string   `json:"displayName"`
	Description  map[string]string   `json:"description"`
	QuickPrompts []map[string]string `json:"quickPrompts,omitempty"`
	Members      []string            `json:"members,omitempty"`
	Greeting     map[string]string   `json:"greeting,omitempty"`
}

type expertWire struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	EntryAgent   string              `json:"entryAgent"`
	DisplayName  map[string]string   `json:"displayName"`
	Description  map[string]string   `json:"description"`
	QuickPrompts []map[string]string `json:"quickPrompts"`
	Members      []string            `json:"members"`
	Greeting     map[string]string   `json:"greeting"`
}

func loadExperts(docs []manifestDoc, agents []AgentFile, ds *diagnostics) []Expert {
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a.Name] = true
	}
	var out []Expert
	state := expertParseState{known: known, seen: map[string]bool{}, seenEntries: map[string]bool{}}
	for _, doc := range docs {
		experts, ok := parseDocumentExperts(doc, ds)
		if !ok {
			continue
		}
		for _, item := range experts {
			entry, ok := parseExpertItem(item, doc.path, state, ds)
			if ok {
				out = append(out, entry)
			}
		}
	}
	return out
}

func parseDocumentExperts(doc manifestDoc, ds *diagnostics) ([]json.RawMessage, bool) {
	raw := doc.wire.Extensions[polarisNamespaceKey]
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var ext struct {
		Experts []json.RawMessage `json:"experts"`
	}
	if err := json.Unmarshal(raw, &ext); err != nil {
		ds.errorf("expert", doc.path, RuleExpertsShape, "extensions[%q] must be an object: %v", polarisNamespaceKey, err)
		return nil, false
	}
	return ext.Experts, true
}

func parseExpertItem(item json.RawMessage, path string, state expertParseState, ds *diagnostics) (Expert, bool) {
	w, err := decodeExpert(item)
	if err != nil || !validExpertMetadata(w, state.known) {
		ds.errorf("expert", path, RuleExpertsShape, "invalid expert metadata entry skipped")
		return Expert{}, false
	}
	key := strings.ToLower(w.ID)
	if state.seen[key] {
		ds.errorf("expert", path, RuleExpertsShape, "duplicate expert id %q skipped", w.ID)
		return Expert{}, false
	}
	if state.seenEntries[w.EntryAgent] {
		ds.errorf("expert", path, RuleExpertsShape, "duplicate expert entryAgent %q skipped", w.EntryAgent)
		return Expert{}, false
	}
	members, ok := expertMembers(w, state.known)
	if !ok || !validQuickPrompts(w.QuickPrompts) {
		ds.errorf("expert", path, RuleExpertsShape, "expert %q has invalid members or prompts", w.ID)
		return Expert{}, false
	}
	state.seen[key], state.seenEntries[w.EntryAgent] = true, true
	return Expert{ID: w.ID, Type: w.Type, EntryAgent: w.EntryAgent, DisplayName: w.DisplayName, Description: w.Description, QuickPrompts: w.QuickPrompts, Members: members, Greeting: w.Greeting}, true
}

func decodeExpert(item json.RawMessage) (expertWire, error) {
	var w expertWire
	dec := json.NewDecoder(bytes.NewReader(item))
	dec.DisallowUnknownFields()
	err := dec.Decode(&w)
	if err != nil {
		return w, apperr.Wrap(apperr.CodeInvalidInput, "expert metadata: decode", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return w, apperr.New(apperr.CodeInvalidInput, "expert metadata: multiple JSON values")
		}
		return w, apperr.Wrap(apperr.CodeInvalidInput, "expert metadata: trailing value", err)
	}
	return w, nil
}

func validExpertMetadata(w expertWire, known map[string]bool) bool {
	return validExpertID(w.ID) && (w.Type == "agent" || w.Type == "team") && known[w.EntryAgent] &&
		validLocalized(w.DisplayName) && validLocalized(w.Description) && (w.Greeting == nil || validLocalized(w.Greeting))
}

func expertMembers(w expertWire, known map[string]bool) ([]string, bool) {
	members := make([]string, 0, len(w.Members))
	seen := make(map[string]bool, len(w.Members))
	for _, member := range w.Members {
		if !known[member] || seen[member] {
			return nil, false
		}
		seen[member] = true
		members = append(members, member)
	}
	if w.Type == "agent" {
		if len(members) > 0 && (len(members) != 1 || members[0] != w.EntryAgent) {
			return nil, false
		}
		return []string{w.EntryAgent}, true
	}
	return members, len(members) > 0 && seen[w.EntryAgent]
}

func validQuickPrompts(prompts []map[string]string) bool {
	if len(prompts) > 5 {
		return false
	}
	for _, prompt := range prompts {
		if !validLocalized(prompt) {
			return false
		}
	}
	return true
}

func validExpertID(id string) bool {
	if id == "" || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	lastDash := false
	for _, r := range id {
		if r == '-' {
			if lastDash {
				return false
			}
			lastDash = true
			continue
		}
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				return false
			}
		}
		lastDash = false
	}
	return true
}

func validLocalized(value map[string]string) bool {
	for lang, text := range value {
		if (lang == "zh" || lang == "en") && strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}
