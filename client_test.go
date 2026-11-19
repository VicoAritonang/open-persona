package openpersona

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHeader(t *testing.T, magic string, appID, format uint32) string {
	t.Helper()
	h := make([]byte, 4096)
	copy(h, magic)
	binary.BigEndian.PutUint32(h[60:], format)
	binary.BigEndian.PutUint32(h[68:], appID)
	path := filepath.Join(t.TempDir(), "x.persona")
	if err := os.WriteFile(path, h, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckPackHeader(t *testing.T) {
	const sqlite = "SQLite format 3\x00"
	for _, tc := range []struct {
		name          string
		magic         string
		appID, format uint32
		wantErr       string
	}{
		{"pack", sqlite, packApplicationID, 1, ""},
		{"pack before the stamp", sqlite, 0, 0, ""},
		{"another database", sqlite, 0x12345678, 1, "application id"},
		{"newer format", sqlite, packApplicationID, packFormat + 1, "pack format"},
		{"not sqlite", "PK\x03\x04", packApplicationID, 1, "not a persona pack"},
	} {
		err := checkPackHeader(writeHeader(t, tc.magic, tc.appID, tc.format))
		if (err == nil) != (tc.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// Error bodies carry a stable code next to the message.
func TestAPIErrorCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"this API key lacks the \"chat\" scope for kora","code":"permission_denied"}`))
	}))
	defer ts.Close()
	_, err := NewClient("k", ClientOptions{BaseURL: ts.URL}).List(context.Background())
	var e *APIError
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Code != "permission_denied" || !strings.Contains(e.Message, "scope") {
		t.Fatalf("got %#v", err)
	}
}
