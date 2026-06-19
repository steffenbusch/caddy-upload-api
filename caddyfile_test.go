package upload

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"12":    12,
		"12B":   12,
		"1KB":   1000,
		"1MiB":  1 << 20,
		"2mb":   2_000_000,
		"1.5KB": 1500,
		"1TB":   1_000_000_000_000,
	}
	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			actual, err := parseSize(input)
			if err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("parseSize(%q) = %d, want %d", input, actual, expected)
			}
		})
	}

	for _, input := range []string{"-1B", "B", "12XB"} {
		t.Run("invalid_"+input, func(t *testing.T) {
			if _, err := parseSize(input); err == nil {
				t.Fatalf("parseSize(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestAPIPathCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		api_path /files
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.APIPath != "/files" {
		t.Fatalf("api_path = %q", h.APIPath)
	}

	for _, value := range []string{"upload", "/", "/files/", "/files//raw", "/files/../raw", "/files/./raw", "/files/.", `/files\raw`, "/files raw"} {
		t.Run(value, func(t *testing.T) {
			if err := validateAPIPath(value); err == nil {
				t.Fatal("invalid api_path was accepted")
			}
		})
	}
}

func TestAllowOverwriteCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		allow_overwrite
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if !h.AllowOverwrite {
		t.Fatal("allow_overwrite was not enabled")
	}

	d = caddyfile.NewTestDispenser(`upload_api {
		allow_overwrite unexpected
	}`)
	if err := h.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("invalid allow_overwrite value was accepted")
	}
}

func TestAllowDotfilesCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		allow_dotfiles
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if !h.AllowDotfiles {
		t.Fatal("allow_dotfiles was not enabled")
	}

	d = caddyfile.NewTestDispenser(`upload_api {
		allow_dotfiles unexpected
	}`)
	if err := h.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("invalid allow_dotfiles value was accepted")
	}
}

func TestTempUploadDirCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		temp_upload_dir /var/tmp/upload-staging
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.TempUploadDir != "/var/tmp/upload-staging" {
		t.Fatalf("temp_upload_dir = %q", h.TempUploadDir)
	}
}

func TestBlockedExtensionsCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		blocked_extensions .php .html .svg
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if got := len(h.BlockedExtensions); got != 3 {
		t.Fatalf("blocked_extensions length = %d", got)
	}
	if h.BlockedExtensions[0] != ".php" || h.BlockedExtensions[2] != ".svg" {
		t.Fatalf("blocked_extensions = %v", h.BlockedExtensions)
	}
}

func TestFilenameReplacementsCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		filename_replacements "ö->oe" "Ö->OE" "ä->ae"
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if got := len(h.FilenameReplacements); got != 3 {
		t.Fatalf("filename_replacements length = %d", got)
	}
	if h.FilenameReplacements[0] != "ö->oe" || h.FilenameReplacements[2] != "ä->ae" {
		t.Fatalf("filename_replacements = %v", h.FilenameReplacements)
	}
}

func TestParseFilenameReplacement(t *testing.T) {
	oldValue, newValue, err := parseFilenameReplacement("ö->oe")
	if err != nil {
		t.Fatal(err)
	}
	if oldValue != "ö" || newValue != "oe" {
		t.Fatalf("parsed replacement = %q -> %q", oldValue, newValue)
	}

	for _, input := range []string{"ö", "->oe", "ö->", "ö->oe->x"} {
		t.Run(input, func(t *testing.T) {
			if _, _, err := parseFilenameReplacement(input); err == nil {
				t.Fatalf("parseFilenameReplacement(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestFilenameErrorCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		filename_error Only ASCII filenames are allowed.
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.FilenameError != "Only ASCII filenames are allowed." {
		t.Fatalf("filename_error = %q", h.FilenameError)
	}
}

func TestFilenamePrefixesCaddyfileOption(t *testing.T) {
	var h UploadAPI
	d := caddyfile.NewTestDispenser(`upload_api {
		filename_prefixes report_ request_
	}`)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if got := len(h.FilenamePrefixes); got != 2 {
		t.Fatalf("filename_prefixes length = %d", got)
	}
	if h.FilenamePrefixes[0] != "report_" || h.FilenamePrefixes[1] != "request_" {
		t.Fatalf("filename_prefixes = %v", h.FilenamePrefixes)
	}
}
