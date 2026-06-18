package upload

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUploadRejectsUnsafeFilenames(t *testing.T) {
	for _, filename := range []string{
		"../../../etc/passwd",
		`..\..\windows\system32.txt`,
		"folder/file.txt",
		`folder\file.txt`,
		"hello world.txt",
		"..",
		".",
		"überblick.txt",
		".env",
	} {
		t.Run(filename, func(t *testing.T) {
			h := newTestHandler(t)
			rec := serve(t, h, multipartRequest(t, map[string]string{filename: "hello"}))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAllowDotfilesEnablesDotfileUploads(t *testing.T) {
	h := newTestHandler(t)
	h.allowAllExtensions = true
	h.extensions = map[string]struct{}{}
	h.AllowDotfiles = true

	rec := serve(t, h, multipartRequest(t, map[string]string{".env": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.UploadDir, ".env")); err != nil {
		t.Fatal(err)
	}
}

func TestFilenameRejectsNamesLongerThan255Bytes(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameRegex = `^[\p{L}\p{N}._+(),=: -]+$`
	h.filenameRE = regexpMustCompile(t, h.FilenameRegex)

	tooLong := strings.Repeat("ä", 126) + ".txt"
	rec := serve(t, h, multipartRequest(t, map[string]string{tooLong: "hello"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "255 bytes") {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestFilenameReplacementsRenameUploadAndResponse(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameReplacements = []string{"ö->oe", "ä->ae"}
	h.filenameReplacementRules = []filenameReplacement{{Old: "ö", New: "oe"}, {Old: "ä", New: "ae"}}

	rec := serve(t, h, multipartRequest(t, map[string]string{"Börse_ä.csv": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.UploadDir, "Boerse_ae.csv")); err != nil {
		t.Fatal(err)
	}
	var response uploadResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Renamed || response.OriginalFilename != "Börse_ä.csv" || response.Filename != "Boerse_ae.csv" {
		t.Fatalf("response = %+v", response)
	}
}

func TestCustomFilenameRegexCanAllowUnicode(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameRegex = `^[\p{L}\p{N}._+(),=: -]+$`
	h.filenameRE = regexpMustCompile(t, h.FilenameRegex)

	rec := serve(t, h, multipartRequest(t, map[string]string{"Umlaut-ä-ö-ü-testfile.csv": "hello"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.UploadDir, "Umlaut-ä-ö-ü-testfile.csv")); err != nil {
		t.Fatal(err)
	}
}

func TestFilenameRegexMustMatchEntireFilename(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameRegex = `report`
	h.filenameRE = regexpMustCompile(t, h.FilenameRegex)
	rec := serve(t, h, multipartRequest(t, map[string]string{"my-report.txt": "hello"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `required pattern \"report\"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestFilenameRegexCanReturnConfiguredUserMessage(t *testing.T) {
	h := newTestHandler(t)
	h.FilenameRegex = `^report_[0-9]{8}\.csv$`
	h.FilenameError = "The file name must be in the format report_YYYYMMDD.csv."
	h.filenameRE = regexpMustCompile(t, h.FilenameRegex)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)

	rec := serve(t, h, multipartRequest(t, map[string]string{"not-valid.csv": "hello"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), h.FilenameError) {
		t.Fatalf("body = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), h.FilenameRegex) {
		t.Fatalf("body leaked regex: %s", rec.Body)
	}

	entries := logs.FilterMessage("upload request rejected").All()
	if len(entries) != 1 {
		t.Fatalf("logs = %+v", logs.All())
	}
	context := entries[0].ContextMap()
	if context["filename_regex"] != h.FilenameRegex {
		t.Fatalf("log context = %+v", context)
	}
	if context["filename_error"] != h.FilenameError {
		t.Fatalf("log context = %+v", context)
	}
}

func TestUploadErrorListsAllowedExtensions(t *testing.T) {
	h := newTestHandler(t)
	rec := serve(t, h, multipartRequest(t, map[string]string{"report.pdf": "hello"}))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `allowed extensions: .csv, .txt`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

type countingBody struct {
	reads int
}

func (b *countingBody) Read(_ []byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func (b *countingBody) Close() error { return nil }
