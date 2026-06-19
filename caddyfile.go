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
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

// Initialize the module by registering it with Caddy and the Caddyfile adapter.
// The directive is ordered before file_server so simple Caddyfiles work even
// without an explicit route block to control handler order manually.
func init() {
	caddy.RegisterModule(UploadAPI{})
	httpcaddyfile.RegisterHandlerDirective("upload_api", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("upload_api", httpcaddyfile.Before, "file_server")
}

// parseCaddyfile parses the upload_api directive for Caddyfile configs.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler = new(UploadAPI)
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return handler, nil
}

// UnmarshalCaddyfile parses the upload_api directive.
func (h *UploadAPI) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for d.NextBlock(0) {
		switch d.Val() {
		case "api_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.APIPath = d.Val()
		case "upload_dir":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.UploadDir = d.Val()
		case "temp_upload_dir":
			// Optional staging directory for temporary .upload-* files before the
			// final atomic move into upload_dir.
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.TempUploadDir = d.Val()
		case "workspace_dir":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.WorkspaceDir = d.Val()
		case "quota":
			value, err := nextSize(d, "quota")
			if err != nil {
				return err
			}
			h.Quota = value
		case "min_size":
			value, err := nextSize(d, "min_size")
			if err != nil {
				return err
			}
			h.MinSize = value
		case "max_size":
			value, err := nextSize(d, "max_size")
			if err != nil {
				return err
			}
			h.MaxSize = value
		case "allowed_extensions":
			extensions := d.RemainingArgs()
			if len(extensions) == 0 {
				return d.ArgErr()
			}
			h.AllowedExtensions = append(h.AllowedExtensions, extensions...)
		case "blocked_extensions":
			extensions := d.RemainingArgs()
			if len(extensions) == 0 {
				return d.ArgErr()
			}
			h.BlockedExtensions = append(h.BlockedExtensions, extensions...)
		case "filename_regex":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.FilenameRegex = d.Val()
		case "filename_error":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			h.FilenameError = strings.Join(args, " ")
		case "filename_prefixes":
			prefixes := d.RemainingArgs()
			if len(prefixes) == 0 {
				return d.ArgErr()
			}
			h.FilenamePrefixes = append(h.FilenamePrefixes, prefixes...)
		case "filename_replacements":
			rules := d.RemainingArgs()
			if len(rules) == 0 {
				return d.ArgErr()
			}
			h.FilenameReplacements = append(h.FilenameReplacements, rules...)
		case "allow_dotfiles":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.AllowDotfiles = true
		case "allow_overwrite":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.AllowOverwrite = true
		default:
			return d.Errf("unknown upload_api option %q", d.Val())
		}
	}
	return nil
}

// nextSize reads one Caddyfile argument and parses the human-readable byte size.
func nextSize(d *caddyfile.Dispenser, option string) (int64, error) {
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	value, err := parseSize(d.Val())
	if err != nil {
		return 0, d.Errf("parsing %s: %v", option, err)
	}
	return value, nil
}

// parseSize delegates to the same human-readable byte parser used elsewhere in
// Caddy and keeps the result within int64 because the handler stores sizes as
// signed byte counts.
func parseSize(value string) (int64, error) {
	size, err := humanize.ParseBytes(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	if size > uint64(^uint64(0)>>1) {
		return 0, errors.New("size exceeds int64 range")
	}
	return int64(size), nil
}

var _ caddyfile.Unmarshaler = (*UploadAPI)(nil)
