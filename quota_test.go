package upload

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadValidatesSizeAndQuota(t *testing.T) {
	t.Run("minimum", func(t *testing.T) {
		h := newTestHandler(t)
		h.MinSize = 10
		rec := serve(t, h, multipartRequest(t, map[string]string{"small.txt": "tiny"}))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "got 4 bytes, minimum is 10 bytes") {
			t.Fatalf("body = %s", rec.Body)
		}
	})

	t.Run("maximum", func(t *testing.T) {
		h := newTestHandler(t)
		h.MaxSize = 4
		rec := serve(t, h, multipartRequest(t, map[string]string{"large.txt": "large"}))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "maximum size of 4 bytes") {
			t.Fatalf("body = %s", rec.Body)
		}
	})

	t.Run("quota", func(t *testing.T) {
		h := newTestHandler(t)
		h.Quota = 4
		rec := serve(t, h, multipartRequest(t, map[string]string{"large.txt": "large"}))
		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "upload is 5 bytes, but only 4 of 4 bytes are free") {
			t.Fatalf("body = %s", rec.Body)
		}
	})
}

func TestQuotaIsCalculatedFromFilesystemOnEveryRequest(t *testing.T) {
	h := newTestHandler(t)
	path := filepath.Join(h.WorkspaceDir, "existing.bin")
	if err := os.WriteFile(path, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := serve(t, h, httptest.NewRequest(http.MethodGet, "/upload/quota", nil))
	if !strings.Contains(first.Body.String(), `"used":4`) {
		t.Fatalf("first response = %s", first.Body)
	}
	if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := serve(t, h, httptest.NewRequest(http.MethodGet, "/upload/quota", nil))
	if !strings.Contains(second.Body.String(), `"used":8`) {
		t.Fatalf("second response = %s", second.Body)
	}
	if second.Header().Get("Cache-Control") != "" || second.Header().Get("ETag") != "" {
		t.Fatalf("unexpected cache headers: %v", second.Header())
	}
}
