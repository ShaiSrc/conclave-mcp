package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

const testRoomID = "01M72DWQRAZXY3X8HWGJ0QZGNW"

func roomsAPI(t *testing.T) (*client.Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/rooms" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"rooms":[{"id":%q,"name":"acme/widgets"}]}`, testRoomID)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.Options{Server: srv.URL, Token: "tk"})
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestRoomID_IDNameEnvAndRepo(t *testing.T) {
	ctx := context.Background()
	c, calls := roomsAPI(t)
	t.Setenv("CONCLAVE_ROOM", "")

	if got, err := RoomID(ctx, c, testRoomID); err != nil || got != testRoomID || *calls != 0 {
		t.Errorf("id: got %q, %v after %d calls; want the id, no lookup", got, err, *calls)
	}
	if got, err := RoomID(ctx, c, "acme/widgets"); err != nil || got != testRoomID {
		t.Errorf("name: got %q, %v", got, err)
	}
	if _, err := RoomID(ctx, c, "acme/other"); err == nil {
		t.Error("unknown name: want an error")
	}

	t.Setenv("CONCLAVE_ROOM", "acme/widgets")
	if got, err := RoomID(ctx, c, ""); err != nil || got != testRoomID {
		t.Errorf("CONCLAVE_ROOM: got %q, %v", got, err)
	}

	t.Setenv("CONCLAVE_ROOM", "")
	dir := gitRepo(t, "git@github.com:acme/ignored.git")
	cfg, _ := json.Marshal(Config{Room: "acme/widgets"})
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir))
	if got, err := RoomID(ctx, c, ""); err != nil || got != testRoomID {
		t.Errorf(".conclave.json: got %q, %v", got, err)
	}

	t.Chdir(t.TempDir()) // no repository, no env
	if _, err := RoomID(ctx, c, ""); err == nil || !strings.Contains(err.Error(), "CONCLAVE_ROOM") {
		t.Errorf("no room anywhere: want a hint at CONCLAVE_ROOM, got %v", err)
	}
}
