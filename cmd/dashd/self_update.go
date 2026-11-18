package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const githubLatestReleaseURL = "https://api.github.com/repos/tonym128/dashd/releases/latest"

type ghRelease struct {
	TagName string `json:"tag_name"`
}

func runSelfUpdate(ctx context.Context, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	return selfUpdateFrom(ctx, githubLatestReleaseURL, "https://github.com/tonym128/dashd/releases/download", version, runtime.GOOS, runtime.GOARCH, exe, out)
}

func selfUpdateFrom(ctx context.Context, apiURL, downloadBase, curVer, goos, goarch, exePath string, out io.Writer) error {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dashd")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check latest release: status %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return fmt.Errorf("decode release metadata: %w", err)
	}
	if rel.TagName == "" {
		return fmt.Errorf("release metadata missing tag_name")
	}
	ver := strings.TrimPrefix(rel.TagName, "v")
	if curVer != "" && curVer != "dev" && strings.TrimPrefix(curVer, "v") == ver {
		fmt.Fprintf(out, "dashd is already up to date (%s)\n", rel.TagName)
		return nil
	}

	ext := "tar.gz"
	binName := "dashd"
	if goos == "windows" {
		ext = "zip"
		binName = "dashd.exe"
	}
	archive := fmt.Sprintf("dashd_%s_%s_%s.%s", ver, goos, goarch, ext)
	url := fmt.Sprintf("%s/%s/%s", strings.TrimRight(downloadBase, "/"), rel.TagName, archive)

	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	dreq.Header.Set("User-Agent", "dashd")
	dresp, err := client.Do(dreq)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, dresp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(dresp.Body, 128<<20))
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}

	var binBytes []byte
	if ext == "zip" {
		binBytes, err = extractFromZip(raw, binName)
	} else {
		binBytes, err = extractFromTarGz(raw, binName)
	}
	if err != nil {
		return err
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, "dashd-update-*")
	if err != nil {
		return fmt.Errorf("create temp binary in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(binBytes); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if goos == "windows" {
		oldPath := exePath + ".old"
		_ = os.Remove(oldPath)
		_ = os.Rename(exePath, oldPath)
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		return fmt.Errorf("replace %s: %w", exePath, err)
	}
	cleanup = false
	fmt.Fprintf(out, "updated %s to %s\n", exePath, rel.TagName)
	return nil
}

func extractFromTarGz(raw []byte, wantName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == wantName {
			return io.ReadAll(io.LimitReader(tr, 128<<20))
		}
	}
	return nil, fmt.Errorf("%s not found in tar.gz archive", wantName)
}

func extractFromZip(raw []byte, wantName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) == wantName {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 128<<20))
		}
	}
	return nil, fmt.Errorf("%s not found in zip archive", wantName)
}
