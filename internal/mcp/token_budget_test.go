package mcp

import (
	"encoding/json"
	"testing"
)

// TokenBudgetGuard prevents regression: the tools/list and instructions payloads
// must stay within defined limits. These values are injected into the model
// context every turn, so every byte matters.
func TestTokenBudgetGuard(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.RegisterSSHConfigWriteTools()

	tools := s.mcpServer.ListTools()
	if len(tools) != 30 {
		t.Fatalf("expected 30 tools, got %d", len(tools))
	}

	total := 0
	descBytes := 0
	propDescBytes := 0
	for _, st := range tools {
		b, err := json.Marshal(st.Tool)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
		descBytes += len(st.Tool.Description)
		props := st.Tool.InputSchema.Properties
		for _, pv := range props {
			pm, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			if d, ok := pm["description"].(string); ok {
				propDescBytes += len(d)
			}
		}
	}
	instructionsLen := len(mcpServerInstructions)

	t.Logf("tools/list total: %d B", total)
	t.Logf("tool descriptions: %d B", descBytes)
	t.Logf("property descriptions: %d B", propDescBytes)
	t.Logf("instructions: %d B", instructionsLen)

	// Budgets (bytes, rough 4:1 B:tokl ratio for English text)
	if total > 25000 {
		t.Errorf("tools/list total %d B exceeds 25000 B budget", total)
	}
	if descBytes > 4000 {
		t.Errorf("tool descriptions %d B exceeds 4000 B budget", descBytes)
	}
	if propDescBytes > 4000 {
		t.Errorf("property descriptions %d B exceeds 4000 B budget", propDescBytes)
	}
	if instructionsLen > 2400 {
		t.Errorf("instructions %d B exceeds 2400 B budget", instructionsLen)
	}
}
