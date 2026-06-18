# Caddy Upload API

The **Caddy Upload API** plugin for [Caddy](https://caddyserver.com) provides a small HTTP API for accepting exactly one multipart file upload per request, reporting current workspace quota usage, and exposing upload limits for frontend preflight checks.

[![Go Report Card](https://goreportcard.com/badge/github.com/steffenbusch/caddy-upload-api)](https://goreportcard.com/report/github.com/steffenbusch/caddy-upload-api)

## Features

This plugin introduces a middleware that:

- **Accepts Single-File Uploads**: Exactly one multipart file per request.
- **Enforces Filesystem Quota**: Quota is calculated from the complete configured workspace directory.
- **Streams to Temporary Files**: Uploads are written to a temporary file first and then moved into place atomically.
- **Validates Filenames and Extensions**: Path traversal, control characters, blocked extensions, and size violations are rejected.
- **Returns JSON Only**: All API responses are JSON with stable response shapes.
- **Avoids Hidden State**: No database, no background jobs, no watchers, and no in-memory quota cache.

### Key Capabilities

- **Quota Endpoint**: Returns current `total`, `used`, and `free` bytes directly from the filesystem.
- **Config Endpoint**: Exposes `min_size`, `max_size`, `allowed_extensions`, `filename_regex`, and optional `filename_error` for frontend validation.
- **Optional Overwrite Support**: Existing regular files can be replaced atomically when explicitly enabled.
- **Configurable Filename Policy**: Use a strict default ASCII-safe pattern or provide your own regex, including Unicode if desired.
- **Structured Logging**: Successful uploads, rejections, and internal errors are logged with request metadata.

## Request Flow

1. **Request Reaches `upload_api`**
   The middleware is mounted on a route such as `/upload*` and handles three endpoints below `api_path`.

2. **Upload Request Validation**
   `POST <api_path>` must be `multipart/form-data` and must contain exactly one file part.

3. **Filename and Extension Checks**
   The raw client filename is sanitized to a basename, then validated against traversal rules, UTF-8/control character rules, the dotfile policy, the configured regex, and the extension policy.

4. **Temporary Write**
   The file body is streamed into a temporary file inside `temp_upload_dir` (or `upload_dir` if no separate temp directory is configured).

5. **Quota Check**
   Workspace size is recalculated directly from the filesystem. The upload is accepted only if:
   `current workspace size + upload size <= quota`

6. **Atomic Store**
   The temporary file is moved into `upload_dir` atomically. Without overwrite mode, existing files are rejected with `409 Conflict`.

7. **JSON Response**
   The handler returns JSON describing success or failure. On success, the response includes current quota values recalculated from the filesystem.

## Configuration Options

- **`api_path`**: Base path of the JSON API.
  - Default: `/upload`
  - The middleware serves `<api_path>`, `<api_path>/quota`, and `<api_path>/config`.

- **`upload_dir`**: Directory where accepted uploads are stored permanently.
  - Required.
  - Created during provisioning if it does not exist.

- **`temp_upload_dir`**: Optional staging directory for temporary upload files.
  - Default: `upload_dir`
  - Should not be exposed via `file_server` or browse.
  - Should be on the same filesystem as `upload_dir` for atomic storage.

- **`workspace_dir`**: Directory whose complete recursive size is used for quota calculations.
  - Required.
  - Must already exist.

- **`quota`**: Maximum allowed workspace size.
  - Required.
  - Parsed using `humanize.ParseBytes`, like Caddy.

- **`min_size`**: Minimum accepted upload size.
  - `0B` allows empty files.

- **`max_size`**: Maximum accepted upload size.
  - Required.
  - Large known requests may be rejected early with `413` before the full body is read.

- **`allowed_extensions`**: Extension allowlist or `*`.
  - Validation is case-insensitive.
  - `*` allows all extensions except those on the active blocked security list.

- **`blocked_extensions`**: Optional replacement for the built-in default blocked security list.
  - Validation is case-insensitive.
  - If omitted, the secure default list is used.
  - If set, it completely replaces the default list.

- **`allow_dotfiles`**: Enables filenames that start with a leading dot, such as `.env`.
  - Disabled by default.
  - Without this flag, dotfiles are rejected before regex and extension checks.

- **`allow_overwrite`**: Enables atomic replacement of existing regular files.
  - Disabled by default.
  - If not set, existing targets return `409 Conflict`.

- **`filename_regex`**: Full filename regex applied after path sanitation.
  - Default: `^[A-Za-z0-9._+-]+$`
  - The default remains intentionally ASCII-only.
  - A custom regex can intentionally allow Unicode filenames.

- **`filename_error`**: Optional user-facing error message returned when the filename does not match `filename_regex`.
  - If unset, the API returns a technical regex-based message.

- **`filename_replacements`**: Ordered server-side filename replacements in the form `old->new`.
  - Applied after client path sanitation and before dotfile, regex, and extension checks.
  - Useful for normalizing names such as `ö->oe` or `ä->ae`.
  - The final stored filename is reported back in the upload response.

### Example Configuration (Caddyfile)

```caddyfile
:8080 {
  route {
    upload_api /upload* {
      api_path /upload
      temp_upload_dir /data/temp-uploads
      upload_dir /data/uploads
      workspace_dir /data
      quota 1GiB
      min_size 12B
      max_size 100MiB
      allowed_extensions *
      blocked_extensions .php .html .svg .exe
      allow_dotfiles
      filename_replacements "ö->oe" "Ö->OE" "ä->ae"
      filename_regex "^[A-Za-z0-9._+-]+$"
      filename_error "The file name may only contain letters, numbers, dots, underscores, plus signs, and dashes."
      allow_overwrite
    }
  }
}
```

## Size Parsing

Sizes are parsed with `humanize.ParseBytes`, matching Caddy conventions.

Examples:

- `12B`
- `1KB`
- `100MB`
- `1GB`
- `1TiB`

`KB`, `MB`, `GB`, and `TB` are decimal units. `KiB`, `MiB`, `GiB`, and `TiB` are binary units.

## HTTP API

All API responses use `Content-Type: application/json`. The handler does not
emit cache headers or ETags for these endpoints.

### `POST <api_path>`

Accepts exactly one multipart file.

Successful response:

```json
{
  "success": true,
  "filename": "example.csv",
  "renamed": false,
  "size": 12345,
  "overwritten": false,
  "quota": {
    "total": 1073741824,
    "used": 123456789,
    "free": 950285035
  }
}
```

After the atomic store, `used` is recalculated directly from `workspace_dir`
and `free` is never negative. If `filename_replacements` changed the final
stored name, the success response also includes `"renamed": true` and
`"original_filename": "..."`.

Error response shape:

```json
{
  "success": false,
  "error": "quota exceeded"
}
```

Status codes:

| Status | Typical cause |
| --- | --- |
| `400 Bad Request` | Invalid multipart request, not exactly one file, unsafe filename including forbidden dotfiles, filename longer than 255 bytes, regex mismatch, or file smaller than `min_size`. |
| `405 Method Not Allowed` | Method other than `POST`; the response includes `Allow: POST`. |
| `409 Conflict` | The target file already exists and overwrite is disabled. |
| `413 Payload Too Large` | File larger than `max_size` or request body exceeds the upload limit. |
| `415 Unsupported Media Type` | File extension is not allowed. |
| `507 Insufficient Storage` | Current workspace size plus upload size exceeds `quota`. |
| `500 Internal Server Error` | Temporary file, filesystem traversal, write, or rename operation failed. |

Representative validation errors:

```json
{"success":false,"error":"file is too small: got 11 bytes, minimum is 12 bytes"}
{"success":false,"error":"filename must match required pattern \"^report_[0-9]{8}\\.csv$\""}
{"success":false,"error":"dotfiles are not allowed"}
{"success":false,"error":"filename must not exceed 255 bytes"}
{"success":false,"error":"file extension \".exe\" is blocked for security reasons"}
{"success":false,"error":"quota exceeded: upload is 500 bytes, but only 120 of 1000000000 bytes are free"}
```

Example with `curl`:

```sh
curl -F 'file=@./report.csv' http://localhost:8080/upload
```

### `GET <api_path>/quota`

Returns current quota values calculated from the filesystem.

The handler traverses the full configured `workspace_dir` on every request.

```json
{
  "quota": {
    "total": 1073741824,
    "used": 123456789,
    "free": 950285035
  }
}
```

Errors:

- `405 Method Not Allowed` for methods other than `GET`; the response includes `Allow: GET`.
- `500 Internal Server Error` if the workspace cannot be fully read.

### `GET <api_path>/config`

Returns the currently effective upload limits for frontend preflight checks.

```json
{
  "min_size": 12,
  "max_size": 100000000,
  "allowed_extensions": ["*"],
  "filename_regex": "^[A-Za-z0-9._+-]+$",
  "filename_error": "The file name may only contain letters, numbers, dots, underscores, plus signs, and dashes."
}
```

This endpoint is intended for frontend preflight checks only. Server-side
validation remains fully authoritative. In particular, `blocked_extensions`,
`filename_replacements`, and the dotfile default are intentionally not exposed
here, and the active blocked security list is never returned.

## Filename Safety

The raw filename from `Content-Disposition` is validated before it is used.

Always rejected:

- empty filenames
- `/`
- `\`
- traversal patterns like `../` and `..\`
- filenames longer than 255 bytes
- dotfiles such as `.env`, unless `allow_dotfiles` is configured
- NUL bytes
- control characters
- invalid UTF-8

The server only ever uses the final basename and never trusts directory components from the client. If `filename_replacements` is configured, those replacements are applied to the sanitized basename before validation and storage.

Examples that are rejected:

- `../../../etc/passwd`
- `..\..\windows\system32`
- `folder/file.txt`
- `folder\file.txt`

Absolute client paths such as `/home/user/report.csv` or `C:\fakepath\report.csv` are reduced to `report.csv` and logged as sanitized input.

## Extension Policy

The plugin applies a blocked extension list even when `allowed_extensions *` is configured.

Blocked extensions include:

```text
.jsp .jspf .jspx .xtp .php .html .xhtml .htm .js .swf .xht .chm .hta
.htc .svg .stm .shtm .shtml .asp .aspx .jnlp .jar .class .cgi .exe .xap
```

The blocked list is applied case-insensitively to the final file extension. Filenames such as `example.html.txt` remain valid because their final extension is `.txt`. When `blocked_extensions` is configured, that configured list fully replaces the built-in defaults.

## Quota Behavior

Quota is always recalculated from the filesystem. The filesystem is the only source of truth.

Before the final move, the rule is:

```text
current workspace size without the temp upload file + upload size <= quota
```

There is no synchronization between parallel instances. Two concurrent uploads may both observe the same free quota and together exceed it. A later quota request will report the actual filesystem state.

## Overwrite Behavior

Without `allow_overwrite`:

- the target must not already exist
- existing files are rejected with `409 Conflict`

With `allow_overwrite`:

- existing regular files are replaced atomically
- the success response includes `"overwritten": true`
- quota calculation accounts for the size of the replaced file

Example successful overwrite response:

```json
{"success":true,"filename":"report.csv","size":12345,"overwritten":true,"quota":{"total":1000000000,"used":123456789,"free":876543211}}
```

When overwrite is disabled and the target already exists:

```json
{"success":false,"error":"file already exists and overwriting is disabled"}
```

## Logging

The plugin uses Caddy's structured logger.

- **Successful uploads** are logged at `info` with fields such as `filename`, absolute `stored_file_path`, `size`, `overwritten`, `host`, `uri`, `remote_addr`, and optional `user_id`.
- **Rejected uploads** are logged at `warn` with HTTP status, error message, request metadata, and relevant context such as `filename`, `filename_regex`, `target_path`, or size information.
- **Internal failures** are logged at `error` with the underlying cause and the same request metadata.

If the Caddy replacer provides `{http.auth.user.id}`, it is included in logs as `user_id`.

## Building with xcaddy

```sh
xcaddy build --with github.com/steffenbusch/caddy-upload-api
```

## License

This project is licensed under the Apache License 2.0. See the [LICENSE](LICENSE) file for details.

## Acknowledgements

- [Caddy](https://caddyserver.com) for providing a powerful and extensible web server.
- [go-humanize](https://github.com/dustin/go-humanize) for human-readable byte parsing and formatting.
