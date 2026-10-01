package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestBridge(apiHandler http.HandlerFunc) *Bridge {
	srv := httptest.NewServer(apiHandler)
	return &Bridge{api: newClient(srv.URL, "test-token")}
}

func rpc(b *Bridge, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.handleMCP(rec, req)
	return rec
}

func TestInitializeEchoesASupportedVersion(t *testing.T) {
	b := newTestBridge(nil)
	rec := rpc(b, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)

	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON-RPC: %v (%s)", err, rec.Body.String())
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned an error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %#v", resp.Result)
	}
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want 2025-06-18", result["protocolVersion"])
	}
}

func TestToolsListReturnsEveryRegisteredTool(t *testing.T) {
	b := newTestBridge(nil)
	rec := rpc(b, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != len(mcpTools) {
		t.Errorf("tools/list returned %d tools, want %d (all registered)", len(tools), len(mcpTools))
	}
}

func TestReadOnlyScopeHidesWritingTools(t *testing.T) {
	b := newTestBridge(nil)
	req := httptest.NewRequest(http.MethodPost, "/mcp?read_only=true", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	b.handleMCP(rec, req)

	var resp jsonRPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	tools := resp.Result.(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "trigger_deploy" || tool["name"] == "apply_migration" {
			t.Errorf("read_only=true still advertised %s", tool["name"])
		}
	}
}

func TestListProjectsCallsTheAPIAndStripsExtraFields(t *testing.T) {
	b := newTestBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"p1","slug":"my-shop","name":"My Shop","status":"active","databasePassword":"should-never-leave-this-mock"}]`))
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`
	rec := rpc(b, body)

	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call returned an error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	text := content["text"].(string)

	if strings.Contains(text, "should-never-leave-this-mock") {
		t.Error("a credential field reached the tool result")
	}
	if !strings.Contains(text, "my-shop") {
		t.Errorf("project slug missing from result: %s", text)
	}
}

func TestScopedConnectionRefusesAToolOutsideItsScope(t *testing.T) {
	b := newTestBridge(nil)
	req := httptest.NewRequest(http.MethodPost, "/mcp?read_only=true",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"trigger_deploy","arguments":{}}}`))
	rec := httptest.NewRecorder()
	b.handleMCP(rec, req)

	var resp jsonRPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Error("a read_only connection was allowed to call trigger_deploy")
	}
}

func TestReadOnlyQueryRejectsWrites(t *testing.T) {
	cases := []struct {
		query   string
		wantErr bool
	}{
		{"SELECT * FROM users", false},
		{"WITH x AS (SELECT 1) SELECT * FROM x", false},
		{"DELETE FROM users", true},
		{"SELECT 1; DROP TABLE users", true},
		{"UPDATE users SET x = 1", true},
		{"INSERT INTO users DEFAULT VALUES", true},
	}
	for _, c := range cases {
		_, err := readOnlyQuery(c.query, 100)
		if (err != nil) != c.wantErr {
			t.Errorf("readOnlyQuery(%q): err = %v, wantErr %v", c.query, err, c.wantErr)
		}
	}
}

func TestTouchesPlatformSchemaCatchesAuthTables(t *testing.T) {
	cases := map[string]bool{
		"CREATE TABLE products (id uuid primary key)":   false,
		"DROP TABLE auth.users":                         true,
		`DROP TABLE "auth".users`:                       true,
		"DELETE FROM my_auth.thing":                     false,
		"ALTER TABLE storage.objects ADD COLUMN x text": true,
	}
	for sql, want := range cases {
		_, got := touchesPlatformSchema(sql)
		if got != want {
			t.Errorf("touchesPlatformSchema(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestApplyMigrationRefusesAPlatformSchema(t *testing.T) {
	// The guard must fire before any write reaches the API - so the mock
	// below only ever needs to answer resolveProject's lookup, and a
	// POST to /database/migrations here would itself be a test failure.
	b := newTestBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the platform-schema guard did not stop a %s to %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p1","slug":"x"}`))
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"apply_migration","arguments":{"project":"x","name":"oops","sql":"DROP TABLE auth.users"}}}`
	rec := rpc(b, body)

	var resp jsonRPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Error("a migration touching auth.users was not refused")
	}
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "platform") {
		t.Errorf("refusal message doesn't explain why: %s", content)
	}
}

func TestAPIErrorBecomesAToolError(t *testing.T) {
	b := newTestBridge(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no project called that"}`))
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_project","arguments":{"project":"nope"}}}`
	rec := rpc(b, body)

	var resp jsonRPCResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Error("a 404 from the API did not become a tool error")
	}
}
