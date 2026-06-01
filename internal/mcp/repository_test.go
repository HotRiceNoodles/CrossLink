package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	testDB  *gorm.DB
	testCtx = context.Background()
)

// createTestTables creates SQLite-compatible schema for MCPServer.
// We use raw SQL instead of AutoMigrate because the model uses
// PostgreSQL-specific "DEFAULT now()" and "type: jsonb" which
// SQLite does not support.
func createTestTables(db *gorm.DB) error {
	return db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_servers (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
				org_id           INTEGER,
			name             TEXT    NOT NULL,
			display_name     TEXT,
			description      TEXT,
			transport_type   TEXT    NOT NULL,
			url              TEXT,
			stdio_config     TEXT,
			auth_type        TEXT    DEFAULT 'none',
			auth_config      TEXT,
			custom_headers   TEXT    DEFAULT '{}',
			status           INTEGER NOT NULL DEFAULT 1,
			health_status    INTEGER NOT NULL DEFAULT 0,
			last_health_check DATETIME,
			tool_count       INTEGER DEFAULT 0,
			enabled          INTEGER DEFAULT 1,
			tier_required    TEXT    DEFAULT 'community',
			created_by       INTEGER,
			created_at       DATETIME NOT NULL,
			updated_at       DATETIME NOT NULL,
			deleted_at       DATETIME
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_servers_name ON mcp_servers(name);
		CREATE INDEX IF NOT EXISTS idx_mcp_servers_deleted_at ON mcp_servers(deleted_at);

		CREATE TABLE IF NOT EXISTS mcp_server_permissions (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			server_id      INTEGER NOT NULL,
			principal_type TEXT    NOT NULL,
			principal_id   INTEGER NOT NULL,
			allow_tools    TEXT    DEFAULT '["*"]',
			deny_tools     TEXT    DEFAULT '[]',
			created_at     DATETIME NOT NULL
		);
			CREATE INDEX IF NOT EXISTS idx_mcp_server_permissions_server_id ON mcp_server_permissions(server_id);

			CREATE TABLE IF NOT EXISTS mcp_tool_call_logs (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				org_id      INTEGER,
				request_id  TEXT    DEFAULT '',
				server_id   INTEGER DEFAULT 0,
				server_name TEXT    DEFAULT '',
				tool_name   TEXT    DEFAULT '',
				method      TEXT    DEFAULT '',
				input_size  INTEGER DEFAULT 0,
				output_size INTEGER DEFAULT 0,
				duration    INTEGER DEFAULT 0,
				status      INTEGER NOT NULL,
				error_code  INTEGER DEFAULT 0,
				error_msg   TEXT    DEFAULT '',
				api_key_id  INTEGER DEFAULT 0,
				user_id     INTEGER DEFAULT 0,
				team_id     INTEGER DEFAULT 0,
				blocked_by  TEXT    DEFAULT '',
				created_at  DATETIME NOT NULL
			);
			CREATE INDEX IF NOT EXISTS idx_mcp_tool_call_logs_request_id ON mcp_tool_call_logs(request_id);
			CREATE INDEX IF NOT EXISTS idx_mcp_tool_call_logs_server_id ON mcp_tool_call_logs(server_id);
			CREATE INDEX IF NOT EXISTS idx_mcp_tool_call_logs_api_key_id ON mcp_tool_call_logs(api_key_id);
			CREATE INDEX IF NOT EXISTS idx_mcp_tool_call_logs_team_id ON mcp_tool_call_logs(team_id);
			CREATE INDEX IF NOT EXISTS idx_mcp_tool_call_logs_created_at ON mcp_tool_call_logs(created_at);
		`).Error
	}

func TestMCPRepo_LogToolCall(t *testing.T) {
	repo := NewMCPRepo(testDB)

	log := &MCPToolCallLog{
		RequestID:  "test-req-1",
		ServerID:   1,
		ServerName: "test-server",
		Method:     MethodToolsCall,
		ToolName:   "search",
		InputSize:  100,
		OutputSize: 200,
		Duration:   50,
		Status:     1,
		CreatedAt:  time.Now(),
	}

	if err := repo.LogToolCall(testCtx, log); err != nil {
		t.Fatalf("LogToolCall: %v", err)
	}
	if log.ID == 0 {
		t.Error("expected ID to be set after insert")
	}
}

func TestMain(m *testing.M) {
	var err error
	testDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	if err := createTestTables(testDB); err != nil {
		panic("createTestTables: " + err.Error())
	}
	os.Exit(m.Run())
}

func TestMCPRepo_Create_and_GetByID(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name:          "test-server",
		DisplayName:   "Test Server",
		TransportType: "http",
		URL:           "http://localhost:8080/mcp",
		AuthType:      "none",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := repo.Create(testCtx, srv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.ID == 0 {
		t.Fatal("expected ID to be set")
	}

	got, err := repo.GetByID(testCtx, 0, srv.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "test-server" {
		t.Errorf("Name = %q, want test-server", got.Name)
	}
}

func TestMCPRepo_List(t *testing.T) {
	repo := NewMCPRepo(testDB)
	servers, err := repo.List(testCtx, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	_ = servers
}

func TestMCPRepo_SoftDelete(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name:          "delete-me",
		TransportType: "http",
		URL:           "http://localhost:9999/mcp",
		AuthType:      "none",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := repo.Create(testCtx, srv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(testCtx, srv.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := repo.GetByID(testCtx, 0, srv.ID)
	if err == nil {
		t.Error("expected error after soft delete, got nil")
	}
}

func TestMCPRepo_GetByName(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name:          "by-name-server",
		TransportType: "http",
		URL:           "http://localhost:8081/mcp",
		AuthType:      "none",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := repo.Create(testCtx, srv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.GetByName(testCtx, 0, "by-name-server")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.ID != srv.ID {
		t.Errorf("ID = %d, want %d", got.ID, srv.ID)
	}
}

func TestMCPRepo_UpdateToolCount(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name:          "toolcount-server",
		TransportType: "http",
		URL:           "http://localhost:8082/mcp",
		AuthType:      "none",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	repo.Create(testCtx, srv)

	if err := repo.UpdateToolCount(testCtx, srv.ID, 42); err != nil {
		t.Fatalf("UpdateToolCount: %v", err)
	}
	got, _ := repo.GetByID(testCtx, 0, srv.ID)
	if got.ToolCount != 42 {
		t.Errorf("ToolCount = %d, want 42", got.ToolCount)
	}
}

func TestMCPRepo_UpdateHealthStatus(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name:          "health-server",
		TransportType: "http",
		URL:           "http://localhost:8083/mcp",
		AuthType:      "none",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	repo.Create(testCtx, srv)

	if err := repo.UpdateHealthStatus(testCtx, srv.ID, 1); err != nil {
		t.Fatalf("UpdateHealthStatus: %v", err)
	}
	got, _ := repo.GetByID(testCtx, 0, srv.ID)
	if got.HealthStatus != 1 {
		t.Errorf("HealthStatus = %d, want 1", got.HealthStatus)
	}
	if got.LastHealthCheck == nil {
		t.Error("expected LastHealthCheck to be set")
	}
}

func TestMCPRepo_DeleteLogsBefore(t *testing.T) {
	repo := NewMCPRepo(testDB)
	old := &MCPToolCallLog{
		RequestID: "old", ServerName: "s", Method: "tools/call",
		Status: 1, CreatedAt: time.Now().Add(-200 * 24 * time.Hour),
	}
	recent := &MCPToolCallLog{
		RequestID: "recent", ServerName: "s", Method: "tools/call",
		Status: 1, CreatedAt: time.Now(),
	}
	repo.LogToolCall(testCtx, old)
	repo.LogToolCall(testCtx, recent)

	cutoff := time.Now().Add(-100 * 24 * time.Hour)
	if err := repo.DeleteLogsBefore(testCtx, cutoff); err != nil {
		t.Fatalf("DeleteLogsBefore: %v", err)
	}

	var count int64
	testDB.Model(&MCPToolCallLog{}).Where("request_id = ?", "old").Count(&count)
	if count != 0 {
		t.Error("expected old log to be deleted")
	}
	testDB.Model(&MCPToolCallLog{}).Where("request_id = ?", "recent").Count(&count)
	if count != 1 {
		t.Error("expected recent log to remain")
	}
}

func TestMCPRepo_Permissions(t *testing.T) {
	repo := NewMCPRepo(testDB)
	srv := &MCPServer{
		Name: "perm-server", TransportType: "http", URL: "http://localhost/mcp",
		AuthType: "none", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	repo.Create(testCtx, srv)

	perm := &MCPServerPermission{
		ServerID:      srv.ID,
		PrincipalType: "key",
		PrincipalID:   99,
		AllowTools:    []byte(`["search","read"]`),
		DenyTools:     []byte(`["delete"]`),
		CreatedAt:     time.Now(),
	}
	if err := repo.CreatePermission(testCtx, perm); err != nil {
		t.Fatalf("CreatePermission: %v", err)
	}

	perms, err := repo.ListPermissions(testCtx, srv.ID)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(perms) != 1 {
		t.Fatalf("got %d permissions, want 1", len(perms))
	}
	if perms[0].PrincipalID != 99 {
		t.Errorf("PrincipalID = %d, want 99", perms[0].PrincipalID)
	}

	if err := repo.DeletePermission(testCtx, perm.ID); err != nil {
		t.Fatalf("DeletePermission: %v", err)
	}
	perms, _ = repo.ListPermissions(testCtx, srv.ID)
	if len(perms) != 0 {
		t.Error("expected no permissions after delete")
	}
}
