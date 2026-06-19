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
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// defaultBlockedExtensionList is the secure default denylist. It applies
	// unless a deployment explicitly overrides it with blocked_extensions.
	defaultBlockedExtensionList = ".jsp .jspf .jspx .xtp .php .html .xhtml .htm .js .swf .xht .chm .hta .htc .svg .stm .shtm .shtml .asp .aspx .jnlp .jar .class .cgi .exe .xap"
)

type filenameValidationError struct {
	message       string
	regexMismatch bool
}

type filenameReplacement struct {
	Old string
	New string
}

func (e *filenameValidationError) Error() string {
	return e.message
}

// sanitizeClientFilename accepts real basenames and absolute client paths from
// legacy upload clients. Relative paths and traversal components are rejected.
func sanitizeClientFilename(filename string) (string, bool, error) {
	if filename == "" {
		return "", false, errors.New("filename is required")
	}
	normalized := strings.ReplaceAll(filename, `\`, "/")
	parts := strings.Split(normalized, "/")
	if slices.Contains(parts, "..") {
		return "", false, errors.New("filename must not contain traversal components")
	}
	if len(parts) == 1 {
		return filename, false, nil
	}
	isWindowsAbsolute := len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/' && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z'))
	if !strings.HasPrefix(normalized, "/") && !isWindowsAbsolute {
		return "", false, errors.New("filename must not contain relative directory paths")
	}
	clean := parts[len(parts)-1]
	if clean == "" || clean == "." {
		return "", false, errors.New("filename is required")
	}
	return clean, true, nil
}

// replaceFilename applies the configured replacements in order to the sanitized
// basename so deployments can normalize client-visible names before further
// validation and storage.
func (h UploadAPI) replaceFilename(filename string) (string, bool) {
	if len(h.filenameReplacementRules) == 0 {
		return filename, false
	}
	current := filename
	for _, rule := range h.filenameReplacementRules {
		current = strings.ReplaceAll(current, rule.Old, rule.New)
	}
	return current, current != filename
}

// parseFilenameReplacement parses an ordered replacement rule in the form
// "old->new". Both sides must be non-empty.
func parseFilenameReplacement(value string) (oldValue, newValue string, err error) {
	parts := strings.Split(value, "->")
	switch len(parts) {
	case 2:
		oldValue = strings.TrimSpace(parts[0])
		newValue = strings.TrimSpace(parts[1])
	default:
		return "", "", fmt.Errorf("expected old->new in %q", value)
	}
	if oldValue == "" {
		return "", "", fmt.Errorf("empty source in %q", value)
	}
	if newValue == "" {
		return "", "", fmt.Errorf("empty target in %q", value)
	}
	return oldValue, newValue, nil
}

// validateFilename enforces the invariant that every later filesystem operation
// receives a single safe basename that matches the configured pattern. The
// default regex keeps the default behavior ASCII-only, but a custom regex may
// intentionally allow Unicode filenames.
func (h UploadAPI) validateFilename(filename string) error {
	if filename == "" || filename == "." || filename == ".." {
		return errors.New("filename is required")
	}
	if strings.Contains(filename, "/") || strings.Contains(filename, `\`) ||
		strings.Contains(filename, "../") || strings.Contains(filename, `..\`) {
		return errors.New("filename must not contain path separators or traversal components")
	}
	if len(filename) > 255 {
		return errors.New("filename must not exceed 255 bytes")
	}
	if !utf8.ValidString(filename) {
		return errors.New("filename must be valid UTF-8")
	}
	if strings.HasPrefix(filename, ".") && !h.AllowDotfiles {
		return errors.New("dotfiles are not allowed")
	}
	for _, char := range filename {
		if char == 0 || unicode.IsControl(char) {
			return errors.New("filename must not contain NUL or control characters")
		}
	}
	if h.filenameRE == nil {
		return errors.New("upload_api module is not provisioned")
	}
	match := h.filenameRE.FindStringIndex(filename)
	if match == nil || match[0] != 0 || match[1] != len(filename) {
		return &filenameValidationError{
			message:       h.filenamePatternError(),
			regexMismatch: true,
		}
	}
	return nil
}

// validateConfiguredFilenamePrefix rejects malformed prefix configuration so
// operators get a clear startup error instead of a silently ineffective rule.
func validateConfiguredFilenamePrefix(prefix string) error {
	if prefix == "" {
		return errors.New("empty prefix")
	}
	if !utf8.ValidString(prefix) {
		return fmt.Errorf("prefix %q must be valid UTF-8", prefix)
	}
	if strings.Contains(prefix, "/") || strings.Contains(prefix, `\`) ||
		strings.Contains(prefix, "../") || strings.Contains(prefix, `..\`) {
		return fmt.Errorf("prefix %q must not contain path separators or traversal components", prefix)
	}
	for _, char := range prefix {
		if char == 0 || unicode.IsControl(char) {
			return fmt.Errorf("prefix %q must not contain NUL or control characters", prefix)
		}
	}
	return nil
}

// validateFilenamePrefix applies the optional allowlist of required leading
// strings after the filename itself has already passed the generic safety and
// regex checks.
func (h UploadAPI) validateFilenamePrefix(filename string) error {
	if len(h.FilenamePrefixes) == 0 {
		return nil
	}
	for _, prefix := range h.FilenamePrefixes {
		if strings.HasPrefix(filename, prefix) {
			return nil
		}
	}
	return errors.New(h.filenamePrefixError())
}

// filenamePattern returns the configured regex or the secure default.
func (h UploadAPI) filenamePattern() string {
	if h.FilenameRegex != "" {
		return h.FilenameRegex
	}
	return defaultFilenamePattern
}

// filenamePatternError returns either the configured user-facing explanation
// or the technical fallback that exposes the active regex.
func (h UploadAPI) filenamePatternError() string {
	if strings.TrimSpace(h.FilenameError) != "" {
		return h.FilenameError
	}
	return fmt.Sprintf("filename must match required pattern %q", h.filenamePattern())
}

// filenamePrefixError keeps the prefix mismatch message readable for end users
// without requiring an additional custom error option.
func (h UploadAPI) filenamePrefixError() string {
	if len(h.FilenamePrefixes) == 1 {
		return fmt.Sprintf("filename must start with %q", h.FilenamePrefixes[0])
	}
	values := make([]string, 0, len(h.FilenamePrefixes))
	for _, prefix := range h.FilenamePrefixes {
		values = append(values, strconv.Quote(prefix))
	}
	return fmt.Sprintf("filename must start with one of: %s", strings.Join(values, ", "))
}

// validateExtension applies the active blacklist first, then the configured
// allowlist or wildcard mode.
func (h UploadAPI) validateExtension(filename string) bool {
	if _, blocked := h.blockedFilenameExtension(filename); blocked {
		return false
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if h.allowAllExtensions {
		return true
	}
	_, ok := h.extensions[extension]
	return ok
}

// extensionError mirrors validateExtension with a user-facing explanation.
func (h UploadAPI) extensionError(filename string) string {
	if extension, blocked := h.blockedFilenameExtension(filename); blocked {
		return fmt.Sprintf("file extension %q is blocked for security reasons", extension)
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if extension == "" {
		extension = "none"
	}
	return fmt.Sprintf("file extension %q is not allowed; allowed extensions: %s", extension, strings.Join(sortedExtensions(h.extensions), ", "))
}

// blockedFilenameExtension checks only the final extension component. This
// keeps the active blocklist focused on the actual file type that will be
// exposed to downstream tooling and avoids rejecting legitimate names such as
// example.html.txt.
func (h UploadAPI) blockedFilenameExtension(filename string) (string, bool) {
	extension := strings.ToLower(filepath.Ext(filename))
	if extension == "" {
		return "", false
	}
	if h.isBlockedExtension(extension) {
		return extension, true
	}
	return "", false
}

// isBlockedExtension checks a normalized extension against the active list.
func (h UploadAPI) isBlockedExtension(extension string) bool {
	if len(h.blockedExtensions) == 0 {
		h.blockedExtensions = buildBlockedExtensions(nil)
	}
	_, blocked := h.blockedExtensions[strings.ToLower(extension)]
	return blocked
}

// buildBlockedExtensions normalizes either the configured override or the
// secure default denylist into a lookup table.
func buildBlockedExtensions(configured []string) map[string]struct{} {
	values := configured
	if len(values) == 0 {
		values = strings.Fields(defaultBlockedExtensionList)
	}
	blocked := make(map[string]struct{}, len(values))
	for _, extension := range values {
		blocked[strings.ToLower(extension)] = struct{}{}
	}
	return blocked
}

// sortedExtensions gives stable JSON output and stable error messages.
func sortedExtensions(extensions map[string]struct{}) []string {
	values := make([]string, 0, len(extensions))
	for extension := range extensions {
		values = append(values, extension)
	}
	sort.Strings(values)
	return values
}
