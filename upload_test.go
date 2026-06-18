package upload

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type nextHandler struct{}

func (nextHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func newTestHandler(t *testing.T) UploadAPI {
	t.Helper()
	workspace := t.TempDir()
	uploadDir := filepath.Join(workspace, "uploads")
	h := UploadAPI{
		APIPath:           defaultAPIPath,
		UploadDir:         uploadDir,
		TempUploadDir:     uploadDir,
		WorkspaceDir:      workspace,
		Quota:             1024,
		MinSize:           1,
		MaxSize:           100,
		AllowedExtensions: []string{".txt", ".CSV"},
		FilenameRegex:     defaultFilenamePattern,
	}
	h.filenameRE = regexpMustCompile(t, h.FilenameRegex)
	h.extensions = map[string]struct{}{".txt": {}, ".csv": {}}
	h.blockedExtensions = buildBlockedExtensions(nil)
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return h
}

func regexpMustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

func multipartRequest(t *testing.T, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, content := range files {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func truncatedMultipartRequest(t *testing.T, filename, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	truncated := body.Bytes()[:body.Len()-8]
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(truncated))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = int64(len(truncated))
	return req
}

func serve(t *testing.T, h UploadAPI, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := h.ServeHTTP(rec, req, caddyhttp.Handler(nextHandler{})); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestUploadSuccess(t *testing.T) {
	h := newTestHandler(t)
	rec := serve(t, h, multipartRequest(t, map[string]string{"report.CSV": "hello"}))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	content, err := os.ReadFile(filepath.Join(h.UploadDir, "report.CSV"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("content = %q", content)
	}
	var response uploadResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Size != 5 || response.Quota.Used != 5 {
		t.Fatalf("response = %+v", response)
	}
}

func TestUploadRejectsMultipleFiles(t *testing.T) {
	h := newTestHandler(t)
	rec := serve(t, h, multipartRequest(t, map[string]string{
		"one.txt": "one",
		"two.txt": "two",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestCustomAPIPathRoutesEndpoints(t *testing.T) {
	h := newTestHandler(t)
	h.APIPath = "/files"

	rec := serve(t, h, multipartRequestAt(t, "/files", map[string]string{"report.txt": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = serve(t, h, httptest.NewRequest(http.MethodGet, "/files/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("quota status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = serve(t, h, httptest.NewRequest(http.MethodGet, "/files/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("config status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = serve(t, h, httptest.NewRequest(http.MethodPost, "/upload", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("default path status = %d", rec.Code)
	}
}

func multipartRequestAt(t *testing.T, target string, files map[string]string) *http.Request {
	req := multipartRequest(t, files)
	req.URL.Path = target
	req.RequestURI = target
	return req
}

func TestOtherPathsCallNextHandler(t *testing.T) {
	h := newTestHandler(t)
	rec := serve(t, h, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestKnownOversizedRequestIsRejectedBeforeReadingBody(t *testing.T) {
	h := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	body := new(countingBody)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	req.ContentLength = h.MaxSize + multipartOverhead + 1

	rec := serve(t, h, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if body.reads != 0 {
		t.Fatalf("request body was read %d times", body.reads)
	}
	if logs.Len() != 1 || logs.All()[0].Message != "upload request rejected" {
		t.Fatalf("logs = %+v", logs.All())
	}
	context := logs.All()[0].ContextMap()
	if context["status"] != int64(http.StatusRequestEntityTooLarge) {
		t.Fatalf("log context = %+v", context)
	}
}

func TestSuccessfulUploadIsLogged(t *testing.T) {
	h := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	rec := serve(t, h, multipartRequest(t, map[string]string{"report.txt": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if logs.Len() != 1 || logs.All()[0].Message != "file uploaded" {
		t.Fatalf("logs = %+v", logs.All())
	}
	context := logs.All()[0].ContextMap()
	if context["filename"] != "report.txt" || context["size"] != int64(5) {
		t.Fatalf("log context = %+v", context)
	}
}

func TestUploadConfigEndpoint(t *testing.T) {
	h := newTestHandler(t)
	rec := serve(t, h, httptest.NewRequest(http.MethodGet, "/upload/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var response configResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.MinSize != h.MinSize || response.MaxSize != h.MaxSize {
		t.Fatalf("response = %+v", response)
	}
	if strings.Join(response.AllowedExtensions, ",") != ".csv,.txt" {
		t.Fatalf("extensions = %v", response.AllowedExtensions)
	}
	if response.FilenameRegex != h.FilenameRegex {
		t.Fatalf("filename regex = %q", response.FilenameRegex)
	}
	if response.FilenameError != "" {
		t.Fatalf("filename error = %q", response.FilenameError)
	}
	if strings.Contains(rec.Body.String(), "blocked_extensions") {
		t.Fatalf("config response exposes blocked extensions: %s", rec.Body)
	}
}

func TestUploadConfigEndpointIncludesFilenameError(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameError = "Use only approved request filenames."

	rec := serve(t, h, httptest.NewRequest(http.MethodGet, "/upload/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var response configResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.FilenameError != h.FilenameError {
		t.Fatalf("response = %+v", response)
	}
}

func TestWildcardExtensionsAllowAllExceptDefaultBlockedList(t *testing.T) {
	for _, filename := range []string{"archive.tar.gz", "data.unknown", "README", "example.html.txt"} {
		t.Run("allowed_"+filename, func(t *testing.T) {
			h := newTestHandler(t)
			h.allowAllExtensions = true
			h.extensions = map[string]struct{}{}
			rec := serve(t, h, multipartRequest(t, map[string]string{filename: "hello"}))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}

	for extension := range strings.FieldsSeq(defaultBlockedExtensionList) {
		filename := "blocked" + strings.ToUpper(extension)
		t.Run("blocked_"+extension, func(t *testing.T) {
			h := newTestHandler(t)
			h.allowAllExtensions = true
			h.extensions = map[string]struct{}{}
			rec := serve(t, h, multipartRequest(t, map[string]string{filename: "hello"}))
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "blocked for security reasons") {
				t.Fatalf("body = %s", rec.Body)
			}
		})
	}
}

func TestExplicitAllowlistCannotOverrideActiveBlockedList(t *testing.T) {
	h := newTestHandler(t)
	h.extensions[".html"] = struct{}{}
	rec := serve(t, h, multipartRequest(t, map[string]string{"page.html": "hello"}))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestCustomBlockedExtensionsReplaceDefaultList(t *testing.T) {
	h := newTestHandler(t)
	h.allowAllExtensions = true
	h.extensions = map[string]struct{}{}
	h.BlockedExtensions = []string{".zip", ".svg"}
	h.blockedExtensions = buildBlockedExtensions(h.BlockedExtensions)

	allowed := serve(t, h, multipartRequest(t, map[string]string{"example.html": "hello"}))
	if allowed.Code != http.StatusCreated {
		t.Fatalf("html status = %d, body = %s", allowed.Code, allowed.Body)
	}

	blocked := serve(t, h, multipartRequest(t, map[string]string{"archive.zip": "hello"}))
	if blocked.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("zip status = %d, body = %s", blocked.Code, blocked.Body)
	}
}

func TestWildcardUploadConfig(t *testing.T) {
	h := newTestHandler(t)
	h.allowAllExtensions = true
	h.extensions = map[string]struct{}{}
	rec := serve(t, h, httptest.NewRequest(http.MethodGet, "/upload/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var response configResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if strings.Join(response.AllowedExtensions, ",") != "*" {
		t.Fatalf("allowed extensions = %v", response.AllowedExtensions)
	}
}

func TestProvisionRejectsInvalidExtensionModes(t *testing.T) {
	newHandler := func(extensions ...string) UploadAPI {
		workspace := t.TempDir()
		return UploadAPI{
			UploadDir:         filepath.Join(workspace, "uploads"),
			WorkspaceDir:      workspace,
			Quota:             1024,
			MaxSize:           100,
			AllowedExtensions: extensions,
			BlockedExtensions: nil,
		}
	}

	t.Run("wildcard combined with allowlist", func(t *testing.T) {
		h := newHandler("*", ".txt")
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("configured blocked extension rejects allowlist entry", func(t *testing.T) {
		h := newHandler(".txt")
		h.BlockedExtensions = []string{".txt"}
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "active security blacklist") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid blocked extension", func(t *testing.T) {
		h := newHandler(".txt")
		h.BlockedExtensions = []string{"txt"}
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "invalid blocked extension") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("blacklist cannot be explicitly allowed", func(t *testing.T) {
		h := newHandler(".PHP")
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "active security blacklist") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid filename replacement", func(t *testing.T) {
		h := newHandler(".txt")
		h.FilenameReplacements = []string{"ö"}
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "invalid filename_replacements") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("duplicate filename replacement source", func(t *testing.T) {
		h := newHandler(".txt")
		h.FilenameReplacements = []string{"ö->oe", "ö->o"}
		if err := h.Provision(caddy.Context{}); err == nil || !strings.Contains(err.Error(), "duplicate source") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("wildcard is valid", func(t *testing.T) {
		h := newHandler("*")
		if err := h.Provision(caddy.Context{}); err != nil {
			t.Fatal(err)
		}
		if !h.allowAllExtensions {
			t.Fatal("wildcard mode was not enabled")
		}
	})
}

func TestUploadOverwriteBehavior(t *testing.T) {
	t.Run("disabled returns conflict", func(t *testing.T) {
		h := newTestHandler(t)
		target := filepath.Join(h.UploadDir, "report.txt")
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := serve(t, h, multipartRequest(t, map[string]string{"report.txt": "new content"}))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "old" {
			t.Fatalf("content = %q", content)
		}
	})

	t.Run("enabled reports overwrite", func(t *testing.T) {
		h := newTestHandler(t)
		h.AllowOverwrite = true
		core, logs := observer.New(zap.InfoLevel)
		h.logger = zap.New(core)
		target := filepath.Join(h.UploadDir, "report.txt")
		if err := os.WriteFile(target, []byte("old content that is longer"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := serve(t, h, multipartRequest(t, map[string]string{"report.txt": "new"}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		var response uploadResponse
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if !response.Overwritten {
			t.Fatalf("response = %+v", response)
		}
		if response.Quota.Used != 3 {
			t.Fatalf("quota = %+v", response.Quota)
		}
		entries := logs.FilterMessage("file uploaded").All()
		if len(entries) != 1 || entries[0].ContextMap()["overwritten"] != true {
			t.Fatalf("logs = %+v", logs.All())
		}
	})
}

func TestClientFilenameSanitation(t *testing.T) {
	for _, rawFilename := range []string{`C:\fakepath\report.txt`, "/home/user/report.txt"} {
		t.Run(rawFilename, func(t *testing.T) {
			h := newTestHandler(t)
			core, logs := observer.New(zap.InfoLevel)
			h.logger = zap.New(core)
			rec := serve(t, h, multipartRequest(t, map[string]string{rawFilename: "hello"}))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if _, err := os.Stat(filepath.Join(h.UploadDir, "report.txt")); err != nil {
				t.Fatal(err)
			}
			if logs.FilterMessage("client filename sanitized").Len() != 1 {
				t.Fatalf("logs = %+v", logs.All())
			}
		})
	}
}

func TestSanitationStillRejectsTraversal(t *testing.T) {
	for _, rawFilename := range []string{"../../report.txt", `..\..\report.txt`} {
		t.Run(rawFilename, func(t *testing.T) {
			h := newTestHandler(t)
			rec := serve(t, h, multipartRequest(t, map[string]string{rawFilename: "hello"}))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestUploadRejectsTruncatedMultipartBody(t *testing.T) {
	h := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	rec := serve(t, h, truncatedMultipartRequest(t, "report.txt", "hello"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "invalid multipart request") {
		t.Fatalf("body = %s", rec.Body)
	}
	if logs.FilterMessage("upload request rejected").Len() != 1 {
		t.Fatalf("logs = %+v", logs.All())
	}
	if logs.FilterMessage("upload request failed").Len() != 0 {
		t.Fatalf("logs = %+v", logs.All())
	}
}

func TestUploadWithSeparateTempUploadDir(t *testing.T) {
	h := newTestHandler(t)
	h.TempUploadDir = filepath.Join(h.WorkspaceDir, "temp-uploads")
	if err := os.MkdirAll(h.TempUploadDir, 0o750); err != nil {
		t.Fatal(err)
	}

	rec := serve(t, h, multipartRequest(t, map[string]string{"report.txt": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.UploadDir, "report.txt")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(h.TempUploadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp_upload_dir not cleaned up: %v", entries)
	}
}
