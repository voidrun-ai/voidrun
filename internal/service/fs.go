package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"voidrun/internal/sandboxclient"
)

type FSService struct {
	client *http.Client
}

func NewFSService() *FSService {
	return &FSService{
		client: sandboxclient.GetSandboxHTTPClient(),
	}
}

// NewFSServiceWithClient creates a filesystem service with a custom client
func NewFSServiceWithClient(client *http.Client) *FSService {
	return &FSService{
		client: client,
	}
}

func (s *FSService) ListFiles(ctx context.Context, sbxID, path string) (*http.Response, error) {
	u := url.URL{
		Scheme:   "http",
		Host:     sbxID,
		Path:     "/ls",
		RawQuery: "path=" + url.QueryEscape(path),
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

// DownloadFile downloads a file from the sandbox
func (s *FSService) DownloadFile(ctx context.Context, sbxID, filePath string) (*http.Response, error) {
	// Ensure path starts with /
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}

	u := url.URL{
		Scheme: "http",
		Host:   sbxID,
		Path:   "/files" + filePath,
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

// UploadFile uploads a file to the sandbox
func (s *FSService) UploadFile(ctx context.Context, sbxID, targetPath string, body io.Reader, contentLength, contentType string) (*http.Response, error) {
	// Ensure path starts with /
	if !strings.HasPrefix(targetPath, "/") {
		targetPath = "/" + targetPath
	}

	u := url.URL{
		Scheme: "http",
		Host:   sbxID,
		Path:   "/upload" + targetPath,
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), body)
	if err != nil {
		return nil, err
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)

	if contentLength != "" {
		if length, err := strconv.ParseInt(contentLength, 10, 64); err == nil {
			req.ContentLength = length
		}
	}

	return s.client.Do(req)
}

// DeleteFile deletes a file on the sandbox
func (s *FSService) DeleteFile(ctx context.Context, sbxID, filePath string) (*http.Response, error) {
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	cmd := fmt.Sprintf("rm -rf '%s'", strings.ReplaceAll(clean, "'", "'\\''"))
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// CreateDirectory creates a directory on the sandbox
func (s *FSService) CreateDirectory(ctx context.Context, sbxID, dirPath string) (*http.Response, error) {
	clean := filepath.Clean(dirPath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	cmd := fmt.Sprintf("mkdir -p '%s'", strings.ReplaceAll(clean, "'", "'\\''"))
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// CreateFile creates a blank file on the sandbox
func (s *FSService) CreateFile(ctx context.Context, sbxID, filePath string) (*http.Response, error) {
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	fileDir := filepath.Dir(clean)
	esc := strings.ReplaceAll(clean, "'", "'\\''")
	dirEsc := strings.ReplaceAll(fileDir, "'", "'\\''")

	cmd := fmt.Sprintf("mkdir -p '%s' && touch '%s'", dirEsc, esc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// MoveFile moves/renames a file on the sandbox
func (s *FSService) MoveFile(ctx context.Context, sbxID, sourcePath, destPath string) (*http.Response, error) {
	src := filepath.Clean(sourcePath)
	dst := filepath.Clean(destPath)
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	if !strings.HasPrefix(dst, "/") {
		dst = "/" + dst
	}

	dstDir := filepath.Dir(dst)
	srcEsc := strings.ReplaceAll(src, "'", "'\\''")
	dstEsc := strings.ReplaceAll(dst, "'", "'\\''")
	dstDirEsc := strings.ReplaceAll(dstDir, "'", "'\\''")

	cmd := fmt.Sprintf("mkdir -p '%s' && mv -f '%s' '%s'", dstDirEsc, srcEsc, dstEsc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// StatFile gets file metadata from the sandbox
func (s *FSService) StatFile(ctx context.Context, sbxID, filePath string) (*http.Response, error) {
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	esc := strings.ReplaceAll(clean, "'", "'\\''")
	cmd := fmt.Sprintf("stat -c '{"+"\"path\":\"%%n\",\"size\":%%s,\"mode\":\"%%f\",\"mtime\":%%Y,\"type\":\"%%F\"}' '%s'", esc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// CopyFile copies/duplicates a file on the sandbox
func (s *FSService) CopyFile(ctx context.Context, sbxID, sourcePath, destPath string) (*http.Response, error) {
	src := filepath.Clean(sourcePath)
	dst := filepath.Clean(destPath)
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	if !strings.HasPrefix(dst, "/") {
		dst = "/" + dst
	}

	dstDir := filepath.Dir(dst)
	srcEsc := strings.ReplaceAll(src, "'", "'\\''")
	dstEsc := strings.ReplaceAll(dst, "'", "'\\''")
	dstDirEsc := strings.ReplaceAll(dstDir, "'", "'\\''")

	cmd := fmt.Sprintf("mkdir -p '%s' && cp -r '%s' '%s'", dstDirEsc, srcEsc, dstEsc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// HeadTail returns first or last N lines of a file
func (s *FSService) HeadTail(ctx context.Context, sbxID, filePath string, lines int, isHead bool) (*http.Response, error) {
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	if lines <= 0 {
		lines = 10
	}
	if lines > 10000 {
		lines = 10000
	}

	esc := strings.ReplaceAll(clean, "'", "'\\''")
	op := "head"
	if !isHead {
		op = "tail"
	}

	cmd := fmt.Sprintf("%s -n %d '%s'", op, lines, esc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// ChangePermissions changes file permissions on the sandbox
func (s *FSService) ChangePermissions(ctx context.Context, sbxID, filePath, mode string) (*http.Response, error) {
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	esc := strings.ReplaceAll(clean, "'", "'\\''")
	modeEsc := strings.ReplaceAll(mode, "'", "'\\''")

	cmd := fmt.Sprintf("chmod '%s' '%s'", modeEsc, esc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// DiskUsage returns disk usage of a directory
func (s *FSService) DiskUsage(ctx context.Context, sbxID, dirPath string) (*http.Response, error) {
	clean := filepath.Clean(dirPath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	esc := strings.ReplaceAll(clean, "'", "'\\''")
	cmd := fmt.Sprintf("du -sh '%s'", esc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// SearchFiles searches for files matching a pattern
func (s *FSService) SearchFiles(ctx context.Context, sbxID, dirPath, pattern string) (*http.Response, error) {
	clean := filepath.Clean(dirPath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	dirEsc := strings.ReplaceAll(clean, "'", "'\\''")
	patternEsc := strings.ReplaceAll(pattern, "'", "'\\''")

	cmd := fmt.Sprintf("find '%s' -name '*%s*' -type f 2>/dev/null | head -100", dirEsc, patternEsc)
	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// CompressFile compresses a file or directory
func (s *FSService) CompressFile(ctx context.Context, sbxID, sourcePath, format string) (*http.Response, error) {
	clean := filepath.Clean(sourcePath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}

	srcEsc := strings.ReplaceAll(clean, "'", "'\\''")
	baseName := filepath.Base(clean)

	var cmd string
	switch format {
	case "tar":
		cmd = fmt.Sprintf("tar -cf '%s.tar' -C '%s' '%s'", clean, filepath.Dir(clean), baseName)
	case "tar.gz":
		cmd = fmt.Sprintf("tar -czf '%s.tar.gz' -C '%s' '%s'", clean, filepath.Dir(clean), baseName)
	case "zip":
		cmd = fmt.Sprintf("zip -r '%s.zip' '%s'", clean, srcEsc)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}

	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// ExtractArchive extracts an archive file
func (s *FSService) ExtractArchive(ctx context.Context, sbxID, archivePath, destPath string) (*http.Response, error) {
	archive := filepath.Clean(archivePath)
	if !strings.HasPrefix(archive, "/") {
		archive = "/" + archive
	}

	dest := filepath.Clean(destPath)
	if !strings.HasPrefix(dest, "/") {
		dest = "/" + dest
	}

	archiveEsc := strings.ReplaceAll(archive, "'", "'\\''")
	destEsc := strings.ReplaceAll(dest, "'", "'\\''")

	var cmd string
	if strings.HasSuffix(archive, ".tar.gz") || strings.HasSuffix(archive, ".tgz") {
		cmd = fmt.Sprintf("mkdir -p '%s' && tar -xzf '%s' -C '%s'", destEsc, archiveEsc, destEsc)
	} else if strings.HasSuffix(archive, ".tar") {
		cmd = fmt.Sprintf("mkdir -p '%s' && tar -xf '%s' -C '%s'", destEsc, archiveEsc, destEsc)
	} else if strings.HasSuffix(archive, ".zip") {
		cmd = fmt.Sprintf("mkdir -p '%s' && unzip -q '%s' -d '%s'", destEsc, archiveEsc, destEsc)
	} else {
		return nil, fmt.Errorf("unsupported archive format")
	}

	payload := map[string]interface{}{"cmd": cmd}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return s.ExecCommand(ctx, sbxID, bytes.NewReader(body))
}

// ExecCommand executes a command on the sandbox
func (s *FSService) ExecCommand(ctx context.Context, sbxID string, body io.Reader) (*http.Response, error) {
	return ExecAgentCommand(ctx, s.client, sbxID, body)
}

// StartWatch starts watching a directory for file changes
func (s *FSService) StartWatch(ctx context.Context, sbxID, path string, recursive bool, ignoreHidden bool) (*http.Response, error) {
	payload := map[string]interface{}{
		"action":       "start",
		"path":         path,
		"recursive":    recursive,
		"ignoreHidden": ignoreHidden,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	u := url.URL{
		Scheme: "http",
		Host:   sbxID,
		Path:   "/watch",
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}
