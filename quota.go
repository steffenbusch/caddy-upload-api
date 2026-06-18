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
	"net/http"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

// newQuota clamps Free at zero so responses stay useful even if the workspace
// already exceeds the configured quota.
func newQuota(total, used int64) quota {
	free := max(total-used, 0)
	return quota{Total: total, Used: used, Free: free}
}

// handleQuota calculates quota from the filesystem for every request.
func (h UploadAPI) handleQuota(w http.ResponseWriter, r *http.Request) {
	used, err := calculateWorkspaceSize(h.WorkspaceDir)
	if err != nil {
		h.respondError(w, r, http.StatusInternalServerError, "could not calculate workspace size", zap.NamedError("cause", err))
		return
	}
	writeJSON(w, http.StatusOK, quotaResponse{Quota: newQuota(h.Quota, used)})
}

// calculateWorkspaceSize walks the workspace every time; the filesystem is the
// source of truth and no quota cache is maintained.
func calculateWorkspaceSize(root string) (int64, error) {
	return calculateWorkspaceSizeExcluding(root, "")
}

// calculateWorkspaceSizeExcluding is used while a temp upload already exists
// but should not count toward the pre-store quota decision.
func calculateWorkspaceSizeExcluding(root, excludedPath string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path != excludedPath && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
