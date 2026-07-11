package schemavalidate

import "testing"

func TestValidate_PlanDAG_Valid(t *testing.T) {
	raw := []byte(`{"nodes":[{"id":"n1","action":"read_file"}],"edges":[]}`)
	if err := Validate("plan_dag", raw); err != nil {
		t.Fatalf("expected valid plan_dag to pass, got: %v", err)
	}
}

func TestValidate_PlanDAG_MissingRequiredNodeField(t *testing.T) {
	// 节点缺少必填的 action 字段——模拟 LLM 幻觉产出结构不完整的场景。
	raw := []byte(`{"nodes":[{"id":"n1"}],"edges":[]}`)
	if err := Validate("plan_dag", raw); err == nil {
		t.Fatal("expected error for node missing required 'action' field, got nil")
	}
}

func TestValidate_PlanDAG_MissingTopLevelField(t *testing.T) {
	raw := []byte(`{"nodes":[]}`) // 缺少 edges
	if err := Validate("plan_dag", raw); err == nil {
		t.Fatal("expected error for missing top-level 'edges' field, got nil")
	}
}

func TestValidate_PlanDAG_WrongType(t *testing.T) {
	// edges 应为 array，此处给了 string。
	raw := []byte(`{"nodes":[],"edges":"not-an-array"}`)
	if err := Validate("plan_dag", raw); err == nil {
		t.Fatal("expected error for edges wrong type, got nil")
	}
}

func TestValidate_PlanDAG_InvalidJSON(t *testing.T) {
	raw := []byte(`{not valid json`)
	if err := Validate("plan_dag", raw); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestValidate_ReflectResult_Valid(t *testing.T) {
	raw := []byte(`{"GoalAchieved":true,"Errors":[],"Learnings":["use safecall.Infer"]}`)
	if err := Validate("reflect_result", raw); err != nil {
		t.Fatalf("expected valid reflect_result to pass, got: %v", err)
	}
}

func TestValidate_ReflectResult_EmptyObjectAllowed(t *testing.T) {
	// ReflectionModel 所有字段都是可选的（零值合法），空对象应当通过。
	raw := []byte(`{}`)
	if err := Validate("reflect_result", raw); err != nil {
		t.Fatalf("expected empty object to pass (no required fields), got: %v", err)
	}
}

func TestValidate_ReflectResult_WrongFieldType(t *testing.T) {
	raw := []byte(`{"GoalAchieved":"yes"}`) // 应为 boolean
	if err := Validate("reflect_result", raw); err == nil {
		t.Fatal("expected error for GoalAchieved wrong type, got nil")
	}
}

func TestValidate_UnregisteredSchema_Passthrough(t *testing.T) {
	// l3_watchdog / perceive_task 未注册到 schemas.json，应当直接放行（跳过校验语义）。
	if err := Validate("l3_watchdog", []byte(`ALLOW`)); err != nil {
		t.Fatalf("expected unregistered schema to pass through, got: %v", err)
	}
	if err := Validate("perceive_task", []byte(`{"anything":"goes"}`)); err != nil {
		t.Fatalf("expected unregistered schema to pass through, got: %v", err)
	}
}

func TestValidate_EmptySchemaRef_Passthrough(t *testing.T) {
	if err := Validate("", []byte(`whatever, not even json`)); err != nil {
		t.Fatalf("expected empty schemaRef to pass through, got: %v", err)
	}
}
