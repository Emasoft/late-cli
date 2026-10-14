package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"late/internal/client"
	"late/internal/session"
	"late/internal/tool"
)

// TestSpawnSubagentTool_ExportedProviderSchemaRetainsFreshSpawnAction captures
// the real Session-to-client HTTP request and compares it with Parameters().
func TestSpawnSubagentTool_ExportedProviderSchemaRetainsFreshSpawnAction(t *testing.T) {
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var err error
		requestBody, err = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read completion request: %v", err)
			http.Error(w, "failed to read request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`); err != nil {
			t.Errorf("write completion response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	c := client.NewClient(client.Config{BaseURL: server.URL, Model: "capture"})
	s := session.New(c, "", nil, "", true)
	spawnTool := tool.SpawnSubagentTool{
		Runner: func(context.Context, tool.SubagentSpawnRequest, *time.Duration) (string, error) {
			return "unused", nil
		},
	}
	s.Registry.Register(spawnTool)

	definitions := s.GetToolDefinitions()
	if len(definitions) != 1 || definitions[0].Function.Name != spawnTool.Name() {
		t.Fatalf("GetToolDefinitions() = %#v, want only %q", definitions, spawnTool.Name())
	}
	rawParameters := spawnTool.Parameters()

	_, err := c.ChatCompletion(t.Context(), client.ChatCompletionRequest{
		Messages: []client.ChatMessage{{Role: "user", Content: client.TextContent("capture schema")}},
		Tools:    definitions,
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}
	if len(requestBody) == 0 {
		t.Fatal("provider received no chat-completion request")
	}

	var captured struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(requestBody, &captured); err != nil {
		t.Fatalf("unmarshal captured provider request: %v", err)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != spawnTool.Name() {
		t.Fatalf("captured tools = %#v, want only %q", captured.Tools, spawnTool.Name())
	}

	var rawSchema, providerSchema any
	if err := json.Unmarshal(rawParameters, &rawSchema); err != nil {
		t.Fatalf("unmarshal raw Parameters(): %v", err)
	}
	if err := json.Unmarshal(captured.Tools[0].Function.Parameters, &providerSchema); err != nil {
		t.Fatalf("unmarshal provider Parameters: %v", err)
	}
	rawJSON, err := json.Marshal(rawSchema)
	if err != nil {
		t.Fatalf("marshal raw Parameters(): %v", err)
	}
	providerJSON, err := json.Marshal(providerSchema)
	if err != nil {
		t.Fatalf("marshal provider Parameters: %v", err)
	}
	if !bytes.Equal(providerJSON, rawJSON) {
		t.Fatalf("provider schema differs from Parameters():\nprovider: %#v\nraw: %#v", providerSchema, rawSchema)
	}

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(captured.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatalf("unmarshal provider tool schema: %v", err)
	}
	if got, want := schema.Properties["action"].Enum, []string{"", tool.SubagentActionFreeze, tool.SubagentActionUnfreeze}; !sameSchemaStrings(got, want) {
		t.Fatalf("provider action enum = %q, want %q", got, want)
	}
	if got, want := schema.Required, []string{"goal", "agent_type"}; !sameSchemaStrings(got, want) {
		t.Fatalf("provider required fields = %q, want only %q", got, want)
	}
}

func sameSchemaStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
