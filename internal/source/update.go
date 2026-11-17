package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// OpenRouterModelsURL is the live OpenRouter model catalogue endpoint.
const OpenRouterModelsURL = "https://openrouter.ai/api/v1/models"

// ModelsFileETag returns the quoted SHA-256 hex digest of path, or "" if
// the file cannot be read.
func ModelsFileETag(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UpdateModels downloads the latest model pricing catalogue from url, validates
// that it contains at least one model entry, and atomically writes it to destPath.
func UpdateModels(ctx context.Context, url, destPath string) (int, error) {
	if url == "" {
		url = OpenRouterModelsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "dashd")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", url, err)
	}
	return WriteModelsBytes(payload, destPath)
}

// WriteModelsBytes validates that payload is a non-empty OpenRouter models JSON
// document and atomically writes it to destPath.
func WriteModelsBytes(payload []byte, destPath string) (int, error) {
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return 0, fmt.Errorf("invalid models JSON: %w", err)
	}
	if len(doc.Data) == 0 {
		return 0, fmt.Errorf("models response contained 0 entries; refusing to overwrite %s", destPath)
	}

	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "models-*.json.tmp")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		return 0, fmt.Errorf("replace %s: %w", destPath, err)
	}
	cleanup = false
	return len(doc.Data), nil
}
