// Copyright 2026 Steffen Busch

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// 	http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

const (
	// defaultAPIPath keeps existing configurations working when api_path is not set.
	defaultAPIPath = "/upload"

	// defaultFilenamePattern is intentionally ASCII-only for predictable,
	// cross-platform filenames unless a site explicitly configures another regex.
	defaultFilenamePattern = `^[A-Za-z0-9._+-]+$`

	// multipartOverhead allows the request-level limiter to include form headers
	// without accepting a file body larger than MaxSize.
	multipartOverhead = int64(1 << 20)
)

var errSizeLimit = errors.New("file exceeds maximum size")

// UploadAPI is a Caddy HTTP middleware module that owns a small upload API.
// The API path defaults to /upload and can be changed with the api_path option.
// It stores exactly one multipart file per request and derives quota information
// directly from the filesystem.
type UploadAPI struct {
	// APIPath is the base path for the upload endpoints. The handler serves
	// APIPath, APIPath/config, and APIPath/quota.
	APIPath string `json:"api_path,omitempty"`

	// UploadDir is the directory where accepted uploads are stored permanently.
	UploadDir string `json:"upload_dir,omitempty"`

	// TempUploadDir is the directory used for temporary upload files before the
	// final atomic move into UploadDir. If empty, UploadDir is used. The
	// staging directory should not be exposed through file_server or browse.
	TempUploadDir string `json:"temp_upload_dir,omitempty"`

	// WorkspaceDir is the directory whose complete recursive size is used for
	// quota calculations. It may contain UploadDir and other application folders.
	WorkspaceDir string `json:"workspace_dir,omitempty"`

	// Quota is the maximum allowed workspace size in bytes.
	Quota int64 `json:"quota,omitempty"`

	// MinSize is the smallest accepted upload size in bytes.
	MinSize int64 `json:"min_size,omitempty"`

	// MaxSize is the largest accepted upload size in bytes.
	MaxSize int64 `json:"max_size,omitempty"`

	// AllowedExtensions contains either an explicit allowlist such as .txt/.csv
	// or a single wildcard entry "*". The active blocklist still applies.
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`

	// BlockedExtensions optionally replaces the built-in default denylist. When
	// empty, defaultBlockedExtensionList is used.
	BlockedExtensions []string `json:"blocked_extensions,omitempty"`

	// FilenameRegex restricts the full basename after path sanitation. If empty,
	// defaultFilenamePattern is used.
	FilenameRegex string `json:"filename_regex,omitempty"`

	// FilenameError optionally overrides the user-facing error returned when the
	// sanitized filename does not match FilenameRegex or the default pattern.
	FilenameError string `json:"filename_error,omitempty"`

	// FilenamePrefixes optionally restricts accepted filenames to a small set of
	// allowed leading strings such as report_ or request_.
	FilenamePrefixes []string `json:"filename_prefixes,omitempty"`

	// FilenameReplacements applies ordered string replacements to sanitized
	// basenames before validation and extension checks.
	FilenameReplacements []string `json:"filename_replacements,omitempty"`

	// AllowDotfiles enables filenames that start with a leading dot, such as
	// .env or .gitignore. Dotfiles are rejected by default.
	AllowDotfiles bool `json:"allow_dotfiles,omitempty"`

	// AllowOverwrite enables atomic replacement of existing regular files. When
	// false, existing targets are rejected with 409 Conflict.
	AllowOverwrite bool `json:"allow_overwrite,omitempty"`

	// filenameRE is compiled during provisioning and is immutable afterwards.
	filenameRE *regexp.Regexp

	// extensions is the normalized lookup table for explicit extension allowlists.
	extensions map[string]struct{}

	// allowAllExtensions is true when allowed_extensions was configured as "*".
	allowAllExtensions bool

	// blockedExtensions is the normalized lookup table for the active denylist.
	blockedExtensions map[string]struct{}

	// filenameReplacementRules keeps the configured filename replacements in the
	// original order so sequential rewrites stay predictable.
	filenameReplacementRules []filenameReplacement

	// logger provides structured logging for upload decisions and failures.
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (UploadAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.upload_api",
		New: func() caddy.Module { return new(UploadAPI) },
	}
}

// Provision validates configuration, applies defaults, prepares immutable
// validation data, and ensures that the upload directory exists.
func (h *UploadAPI) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)
	if h.APIPath == "" {
		h.APIPath = defaultAPIPath
	}
	if err := validateAPIPath(h.APIPath); err != nil {
		return err
	}
	if h.UploadDir == "" {
		return errors.New("upload_dir is required")
	}
	if h.WorkspaceDir == "" {
		return errors.New("workspace_dir is required")
	}
	if h.Quota <= 0 {
		return errors.New("quota must be greater than zero")
	}
	if h.MinSize < 0 {
		return errors.New("min_size must not be negative")
	}
	if h.MaxSize <= 0 {
		return errors.New("max_size must be greater than zero")
	}
	if h.MinSize > h.MaxSize {
		return errors.New("min_size must not exceed max_size")
	}
	if len(h.AllowedExtensions) == 0 {
		return errors.New("allowed_extensions must contain at least one extension")
	}

	pattern := h.FilenameRegex
	if pattern == "" {
		pattern = defaultFilenamePattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("compile filename_regex: %w", err)
	}
	h.filenameRE = re

	h.filenameReplacementRules = make([]filenameReplacement, 0, len(h.FilenameReplacements))
	seenReplacements := make(map[string]struct{}, len(h.FilenameReplacements))
	for _, rule := range h.FilenameReplacements {
		oldValue, newValue, err := parseFilenameReplacement(rule)
		if err != nil {
			return fmt.Errorf("invalid filename_replacements: %w", err)
		}
		if _, exists := seenReplacements[oldValue]; exists {
			return fmt.Errorf("invalid filename_replacements: duplicate source: %s", oldValue)
		}
		seenReplacements[oldValue] = struct{}{}
		h.filenameReplacementRules = append(h.filenameReplacementRules, filenameReplacement{Old: oldValue, New: newValue})
	}

	seenPrefixes := make(map[string]struct{}, len(h.FilenamePrefixes))
	for _, prefix := range h.FilenamePrefixes {
		if err := validateConfiguredFilenamePrefix(prefix); err != nil {
			return fmt.Errorf("invalid filename_prefixes: %w", err)
		}
		if _, exists := seenPrefixes[prefix]; exists {
			return fmt.Errorf("invalid filename_prefixes: duplicate prefix: %s", prefix)
		}
		seenPrefixes[prefix] = struct{}{}
	}

	for _, extension := range h.BlockedExtensions {
		extension = strings.ToLower(extension)
		if !strings.HasPrefix(extension, ".") || len(extension) < 2 {
			return fmt.Errorf("invalid blocked extension %q", extension)
		}
	}

	h.blockedExtensions = buildBlockedExtensions(h.BlockedExtensions)
	h.extensions = make(map[string]struct{}, len(h.AllowedExtensions))
	for _, extension := range h.AllowedExtensions {
		extension = strings.ToLower(extension)
		if extension == "*" {
			if len(h.AllowedExtensions) != 1 {
				return errors.New("allowed_extensions * cannot be combined with individual extensions")
			}
			h.allowAllExtensions = true
			continue
		}
		if !strings.HasPrefix(extension, ".") || len(extension) < 2 {
			return fmt.Errorf("invalid allowed extension %q", extension)
		}
		if h.isBlockedExtension(extension) {
			return fmt.Errorf("allowed extension %q is blocked by the active security blacklist", extension)
		}
		h.extensions[extension] = struct{}{}
	}

	if err := os.MkdirAll(h.UploadDir, 0o750); err != nil {
		return fmt.Errorf("create upload_dir: %w", err)
	}
	// Temporary files may live outside UploadDir so deployments can keep them
	// out of publicly browsable trees while preserving the same upload flow.
	if err := os.MkdirAll(h.tempUploadDir(), 0o750); err != nil {
		return fmt.Errorf("create temp_upload_dir: %w", err)
	}
	tempInfo, err := os.Stat(h.tempUploadDir())
	if err != nil {
		return fmt.Errorf("stat temp_upload_dir: %w", err)
	}
	if !tempInfo.IsDir() {
		return errors.New("temp_upload_dir is not a directory")
	}
	info, err := os.Stat(h.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("stat workspace_dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("workspace_dir is not a directory")
	}
	return nil
}

// Validate performs cross-field checks that depend on provisioned values.
func (h UploadAPI) Validate() error {
	if h.MaxSize > int64(^uint64(0)>>1)-multipartOverhead {
		return errors.New("max_size is too large")
	}
	return nil
}

// ServeHTTP routes requests handled by this middleware. Unknown paths are
// passed to the next Caddy handler so the directive can be mounted by Caddy
// matchers such as /upload* or /documents/upload*.
func (h UploadAPI) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	switch r.URL.Path {
	case h.APIPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			h.respondError(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return nil
		}
		h.handleUpload(w, r)
		return nil
	case h.apiEndpoint("/config"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.respondError(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return nil
		}
		h.handleConfig(w)
		return nil
	case h.apiEndpoint("/quota"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.respondError(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return nil
		}
		h.handleQuota(w, r)
		return nil
	default:
		return next.ServeHTTP(w, r)
	}
}

// apiEndpoint returns a concrete endpoint path below the configured API base.
func (h UploadAPI) apiEndpoint(suffix string) string {
	return h.APIPath + suffix
}

// tempUploadDir returns the staging directory for temporary upload files.
// Keeping the default on UploadDir preserves backward compatibility, while an
// explicit temp_upload_dir lets operators hide in-progress files elsewhere.
func (h UploadAPI) tempUploadDir() string {
	if h.TempUploadDir != "" {
		return h.TempUploadDir
	}
	return h.UploadDir
}

// validateAPIPath ensures the configured base path can be compared exactly in
// ServeHTTP and cannot accidentally produce duplicate endpoint paths.
func validateAPIPath(apiPath string) error {
	if apiPath == "" {
		return errors.New("api_path is required")
	}
	if apiPath == "/" {
		return errors.New("api_path must not be /")
	}
	if !strings.HasPrefix(apiPath, "/") {
		return errors.New("api_path must start with /")
	}
	if strings.HasSuffix(apiPath, "/") {
		return errors.New("api_path must not end with /")
	}
	if strings.Contains(apiPath, "//") || strings.Contains(apiPath, "/../") || strings.HasSuffix(apiPath, "/..") ||
		strings.Contains(apiPath, "/./") || strings.HasSuffix(apiPath, "/.") || strings.Contains(apiPath, `\`) {
		return errors.New("api_path must be a clean URL path")
	}
	for _, char := range []byte(apiPath) {
		if char == 0 || char <= 0x20 || char == 0x7f {
			return errors.New("api_path must not contain whitespace, NUL, or control characters")
		}
	}
	return nil
}

// handleUpload validates one multipart file, writes it to a temporary file,
// checks quota against the current filesystem state, and finally stores it.
func (h UploadAPI) handleUpload(w http.ResponseWriter, r *http.Request) {
	// If the browser sends a known request size that is clearly impossible to
	// accept, reject early before spending time reading the body.
	if r.ContentLength > h.MaxSize+multipartOverhead {
		h.respondError(w, r, http.StatusRequestEntityTooLarge, h.maximumSizeError(),
			zap.Int64("request_size", r.ContentLength), zap.Int64("request_limit", h.MaxSize+multipartOverhead))
		return
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		h.respondError(w, r, http.StatusBadRequest, "multipart/form-data required")
		return
	}

	// MaxBytesReader is still required for clients without a useful
	// Content-Length and for oversized multipart bodies.
	r.Body = http.MaxBytesReader(w, r.Body, h.MaxSize+multipartOverhead)
	reader := multipart.NewReader(r.Body, params["boundary"])

	var tempPath, filename, originalFilename string
	var size int64
	var renamed bool
	fileCount := 0
	// tempPath remains set until the file has been moved or linked into its
	// final location. This keeps failed uploads from leaving partial files behind.
	defer func() {
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()

	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			if isMaxBytesError(nextErr) {
				h.respondError(w, r, http.StatusRequestEntityTooLarge, h.maximumSizeError())
				return
			}
			h.respondError(w, r, http.StatusBadRequest, "invalid multipart request", zap.NamedError("cause", nextErr))
			return
		}

		rawFilename, isFile, parseErr := multipartFilename(part)
		if parseErr != nil {
			_ = part.Close()
			h.respondError(w, r, http.StatusBadRequest, "invalid multipart request", zap.NamedError("cause", parseErr))
			return
		}
		if !isFile {
			// Normal form fields are ignored, but their body still has to be drained so
			// multipart parsing and request-size limits stay consistent.
			if _, copyErr := io.Copy(io.Discard, part); copyErr != nil {
				_ = part.Close()
				if isMaxBytesError(copyErr) {
					h.respondError(w, r, http.StatusRequestEntityTooLarge, h.maximumSizeError())
					return
				}
				h.respondError(w, r, http.StatusBadRequest, "invalid multipart request", zap.NamedError("cause", copyErr))
				return
			}
			_ = part.Close()
			continue
		}

		fileCount++
		if fileCount > 1 {
			_ = part.Close()
			h.respondError(w, r, http.StatusBadRequest, "exactly one file is required")
			return
		}
		cleanFilename, sanitized, err := sanitizeClientFilename(rawFilename)
		if err != nil {
			_ = part.Close()
			h.respondError(w, r, http.StatusBadRequest, err.Error(), zap.String("filename", rawFilename))
			return
		}
		if sanitized && h.logger != nil {
			h.logger.Info("client filename sanitized", h.requestLogFields(r,
				zap.String("original_filename", rawFilename),
				zap.String("filename", cleanFilename))...)
		}
		originalFilename = cleanFilename
		effectiveFilename, filenameReplaced := h.replaceFilename(cleanFilename)
		if filenameReplaced {
			renamed = true
			if h.logger != nil {
				h.logger.Info("client filename replaced", h.requestLogFields(r,
					zap.String("original_filename", cleanFilename),
					zap.String("filename", effectiveFilename))...)
			}
		}
		if err := h.validateFilename(effectiveFilename); err != nil {
			_ = part.Close()
			fields := []zap.Field{zap.String("filename", effectiveFilename)}
			if filenameReplaced {
				fields = append(fields, zap.String("original_filename", cleanFilename))
			}
			var filenameErr *filenameValidationError
			if errors.As(err, &filenameErr) && filenameErr.regexMismatch {
				fields = append(fields, zap.String("filename_regex", h.filenamePattern()))
			}
			h.respondError(w, r, http.StatusBadRequest, err.Error(), fields...)
			return
		}
		if err := h.validateFilenamePrefix(effectiveFilename); err != nil {
			_ = part.Close()
			fields := []zap.Field{zap.String("filename", effectiveFilename)}
			if filenameReplaced {
				fields = append(fields, zap.String("original_filename", cleanFilename))
			}
			h.respondError(w, r, http.StatusBadRequest, err.Error(), fields...)
			return
		}
		if !h.validateExtension(effectiveFilename) {
			_ = part.Close()
			fields := []zap.Field{zap.String("filename", effectiveFilename)}
			if filenameReplaced {
				fields = append(fields, zap.String("original_filename", cleanFilename))
			}
			h.respondError(w, r, http.StatusUnsupportedMediaType, h.extensionError(effectiveFilename), fields...)
			return
		}

		// Write into UploadDir first. The final step below is then an atomic rename
		// or link within the same filesystem directory.
		temp, createErr := os.CreateTemp(h.tempUploadDir(), ".upload-*")
		if createErr != nil {
			_ = part.Close()
			h.respondError(w, r, http.StatusInternalServerError, "could not create temporary file", zap.NamedError("cause", createErr))
			return
		}
		tempPath = temp.Name()
		size, err = copyWithLimit(temp, part, h.MaxSize)
		closeErr := temp.Close()
		_ = part.Close()
		if errors.Is(err, errSizeLimit) || isMaxBytesError(err) {
			h.respondError(w, r, http.StatusRequestEntityTooLarge, h.maximumSizeError())
			return
		}
		if isClientBodyReadError(err) {
			h.respondError(w, r, http.StatusBadRequest, "invalid multipart request", zap.NamedError("cause", err))
			return
		}
		if err != nil {
			h.respondError(w, r, http.StatusInternalServerError, "could not write uploaded file", zap.NamedError("write_error", err), zap.NamedError("close_error", closeErr))
			return
		}
		if closeErr != nil {
			h.respondError(w, r, http.StatusInternalServerError, "could not write uploaded file", zap.NamedError("close_error", closeErr))
			return
		}
		filename = effectiveFilename
	}

	if fileCount != 1 {
		h.respondError(w, r, http.StatusBadRequest, "exactly one file is required")
		return
	}
	if err := h.validateFileSize(size); err != nil {
		status := http.StatusBadRequest
		if size > h.MaxSize {
			status = http.StatusRequestEntityTooLarge
		}
		h.respondError(w, r, status, err.Error(), zap.String("filename", filename), zap.Int64("size", size))
		return
	}

	target := filepath.Join(h.UploadDir, filename)
	absoluteTarget := absolutePath(target)
	replacedSize, targetExists, err := existingRegularFileSize(target)
	if targetExists && !h.AllowOverwrite {
		h.respondError(w, r, http.StatusConflict, "file already exists and overwriting is disabled", zap.String("filename", filename), zap.String("target_path", absoluteTarget))
		return
	}
	if err != nil {
		h.respondError(w, r, http.StatusInternalServerError, "could not inspect upload target", zap.NamedError("cause", err), zap.String("target_path", absoluteTarget))
		return
	}

	// The temporary upload file is excluded because the quota decision is based
	// on the workspace before the final file becomes visible.
	used, err := calculateWorkspaceSizeExcluding(h.WorkspaceDir, tempPath)
	if err != nil {
		h.respondError(w, r, http.StatusInternalServerError, "could not calculate workspace size", zap.NamedError("cause", err))
		return
	}
	// When overwriting, only the size delta consumes additional quota.
	required := size - replacedSize
	if required > h.Quota-used {
		h.respondError(w, r, http.StatusInsufficientStorage, fmt.Sprintf("quota exceeded: upload is %d bytes, but only %d of %d bytes are free", size, max(0, h.Quota-used), h.Quota), zap.String("filename", filename), zap.Int64("size", size), zap.Int64("workspace_used", used))
		return
	}

	overwritten, err := h.storeTemporaryFile(tempPath, target, targetExists)
	if errors.Is(err, os.ErrExist) {
		h.respondError(w, r, http.StatusConflict, "file already exists and overwriting is disabled", zap.String("filename", filename), zap.String("target_path", absoluteTarget))
		return
	}
	// Atomic replacement relies on rename semantics, so staging and target must
	// live on the same filesystem. Crossing devices would force a copy, which the
	// handler intentionally rejects to keep the write path simple and predictable.
	if isCrossDeviceError(err) {
		h.respondError(w, r, http.StatusInternalServerError, "temp_upload_dir must be on the same filesystem as upload_dir for atomic storage", zap.NamedError("cause", err), zap.String("target_path", absoluteTarget), zap.String("temp_upload_dir", absolutePath(h.tempUploadDir())))
		return
	}
	if err != nil {
		h.respondError(w, r, http.StatusInternalServerError, "could not store uploaded file", zap.NamedError("cause", err), zap.String("target_path", absoluteTarget))
		return
	}
	tempPath = ""

	if h.logger != nil {
		fields := []zap.Field{
			zap.String("filename", filename),
			zap.String("stored_file_path", absoluteTarget),
			zap.Int64("size", size),
			zap.Bool("renamed", renamed),
			zap.Bool("overwritten", overwritten),
		}
		if renamed {
			fields = append(fields, zap.String("original_filename", originalFilename))
		}
		h.logger.Info("file uploaded", h.requestLogFields(r, fields...)...)
	}

	currentUsed, err := calculateWorkspaceSize(h.WorkspaceDir)
	if err != nil {
		h.respondError(w, r, http.StatusInternalServerError, "file stored, but quota could not be calculated", zap.NamedError("cause", err), zap.String("target_path", absoluteTarget))
		return
	}
	response := uploadResponse{
		Success:     true,
		Filename:    filename,
		Renamed:     renamed,
		Size:        size,
		Overwritten: overwritten,
		Quota:       newQuota(h.Quota, currentUsed),
	}
	if renamed {
		response.OriginalFilename = originalFilename
	}
	writeJSON(w, http.StatusCreated, response)
}

// handleConfig exposes only the client-side preflight data needed by the demo.
// It deliberately does not publish the mandatory blocked extension list.
func (h UploadAPI) handleConfig(w http.ResponseWriter) {
	extensions := sortedExtensions(h.extensions)
	if h.allowAllExtensions {
		extensions = []string{"*"}
	}
	prefixes := append([]string{}, h.FilenamePrefixes...)
	writeJSON(w, http.StatusOK, configResponse{
		MinSize:           h.MinSize,
		MaxSize:           h.MaxSize,
		AllowedExtensions: extensions,
		FilenameRegex:     h.filenamePattern(),
		FilenameError:     h.FilenameError,
		FilenamePrefixes:  prefixes,
	})
}

// maximumSizeError centralizes the message used for early and streaming limits.
func (h UploadAPI) maximumSizeError() string {
	return fmt.Sprintf("file exceeds maximum size of %d bytes", h.MaxSize)
}

// validateFileSize checks the exact bytes written from the multipart file part.
func (h UploadAPI) validateFileSize(size int64) error {
	if size < h.MinSize {
		return fmt.Errorf("file is too small: got %d bytes, minimum is %d bytes", size, h.MinSize)
	}
	if size > h.MaxSize {
		return fmt.Errorf("file is too large: got %d bytes, maximum is %d bytes", size, h.MaxSize)
	}
	return nil
}

// multipartFilename extracts the client-supplied filename from a form-data part.
func multipartFilename(part *multipart.Part) (string, bool, error) {
	disposition := part.Header.Get("Content-Disposition")
	mediaType, params, err := mime.ParseMediaType(disposition)
	if err != nil || mediaType != "form-data" {
		return "", false, err
	}
	filename, exists := params["filename"]
	return filename, exists, nil
}

// isMaxBytesError detects request-body limit failures returned by net/http.
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

// isClientBodyReadError classifies truncated or canceled upload bodies as
// request problems instead of internal server write failures.
func isClientBodyReadError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled)
}

func isCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// copyWithLimit streams the file part and reads one byte beyond the limit so
// oversized uploads can be rejected without buffering them in memory.
func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, errSizeLimit
	}
	return written, nil
}

// existingRegularFileSize reports whether the target exists and is a regular
// file. Non-regular targets are rejected rather than overwritten.
func existingRegularFileSize(path string) (int64, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, true, errors.New("upload target exists but is not a regular file")
	}
	return info.Size(), true, nil
}

// storeTemporaryFile makes the temporary upload visible at the target path.
// Without overwrite it uses link+remove, so an existing target fails atomically.
func (h UploadAPI) storeTemporaryFile(tempPath, target string, targetExists bool) (bool, error) {
	if h.AllowOverwrite {
		return targetExists, os.Rename(tempPath, target)
	}
	if err := os.Link(tempPath, target); err != nil {
		return false, err
	}
	if err := os.Remove(tempPath); err != nil {
		return false, err
	}
	return false, nil
}

// respondError writes the stable JSON error shape and logs the rejection with
// request context. User errors are warnings; unexpected server failures are errors.
func (h UploadAPI) respondError(w http.ResponseWriter, r *http.Request, status int, message string, fields ...zap.Field) {
	if h.logger != nil {
		fields = h.requestLogFields(r, append(fields,
			zap.Int("status", status),
			zap.String("error", message),
		)...)
		if status >= http.StatusInternalServerError && status != http.StatusInsufficientStorage {
			h.logger.Error("upload request failed", fields...)
		} else {
			h.logger.Warn("upload request rejected", fields...)
		}
	}
	writeJSON(w, status, errorResponse{Success: false, Error: message})
}

// requestLogFields appends common request metadata to every structured log.
func (h UploadAPI) requestLogFields(r *http.Request, fields ...zap.Field) []zap.Field {
	fields = append(fields,
		zap.String("method", r.Method),
		zap.String("uri", r.RequestURI),
		zap.String("host", r.Host),
		zap.String("remote_addr", r.RemoteAddr),
		zap.Int64("content_length", r.ContentLength),
	)
	if userID := requestUserID(r); userID != "" {
		fields = append(fields, zap.String("user_id", userID))
	}
	return fields
}

// requestUserID reads the authenticated user identifier from Caddy's replacer.
func requestUserID(r *http.Request) string {
	replacer, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if !ok || replacer == nil {
		return ""
	}
	userID, ok := replacer.GetString("http.auth.user.id")
	if !ok {
		return ""
	}
	return userID
}

// absolutePath keeps filesystem targets readable even when configured relatively.
func absolutePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

var (
	_ caddy.Module                = (*UploadAPI)(nil)
	_ caddy.Provisioner           = (*UploadAPI)(nil)
	_ caddy.Validator             = (*UploadAPI)(nil)
	_ caddyhttp.MiddlewareHandler = (*UploadAPI)(nil)
)
