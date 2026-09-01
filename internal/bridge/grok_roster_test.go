package bridge

import "testing"

func TestGrokProductRosterResolvesOneResidentSession(t *testing.T) {
	selected := "11111111-1111-4111-8111-111111111111"
	response := map[string]any{"result": map[string]any{"sessions": []any{
		map[string]any{"sessionId": "22222222-2222-4222-8222-222222222222", "resident": false, "activity": "dormant", "yolo": false},
		map[string]any{"sessionId": selected, "resident": true, "activity": "idle", "title": "product title", "yolo": true},
	}}}
	id, state, err := grokSelectedResidentSession(response)
	if err != nil || id != selected || state.name != "product title" || state.permissionMode != "bypassPermissions" {
		t.Fatalf("selection = %q, %+v, %v", id, state, err)
	}
}

func TestGrokProductRosterRejectsAmbiguousResidentSelection(t *testing.T) {
	response := map[string]any{"result": map[string]any{"sessions": []any{
		map[string]any{"sessionId": "11111111-1111-4111-8111-111111111111", "resident": true, "activity": "idle", "yolo": false},
		map[string]any{"sessionId": "22222222-2222-4222-8222-222222222222", "resident": true, "activity": "working", "yolo": false},
	}}}
	if _, _, err := grokSelectedResidentSession(response); err == nil {
		t.Fatal("ambiguous live product selection was accepted")
	}
}
