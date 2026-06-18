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
	"encoding/json"
	"net/http"
)

type quota struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
}

type quotaResponse struct {
	Quota quota `json:"quota"`
}

type configResponse struct {
	MinSize           int64    `json:"min_size"`
	MaxSize           int64    `json:"max_size"`
	AllowedExtensions []string `json:"allowed_extensions"`
	FilenameRegex     string   `json:"filename_regex"`
	FilenameError     string   `json:"filename_error,omitempty"`
}

type uploadResponse struct {
	Success          bool   `json:"success"`
	Filename         string `json:"filename"`
	OriginalFilename string `json:"original_filename,omitempty"`
	Renamed          bool   `json:"renamed"`
	Size             int64  `json:"size"`
	Overwritten      bool   `json:"overwritten"`
	Quota            quota  `json:"quota"`
}

type errorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// writeJSON is intentionally small: every endpoint in this module returns JSON.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
