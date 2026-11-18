package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const githubLatestReleaseURL = "https://api.github.com/repos/tonym128/dashd/releases/latest"

type ghRelease struct {
	TagName string `json:"tag_name"`
}

// runSelfUpdate installs the latest release over the running binary and
// reports whether it changed anything.
func runSelfUpdate(ctx context.Context, out io.Writer) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locate current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return false, fmt.Errorf("resolve executable path: %w", err)
	}
	before := fileDigest(exe)
	if err := selfUpdateFrom(ctx, githubLatestReleaseURL, "https://github.com/tonym128/dashd/releases/download", version, runtime.GOOS, runtime.GOARCH, exe, out); err != nil {
		return false, err
	}
	return fileDigest(exe) != before, nil
}

func fileDigest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// compareVersions compares dotted numeric versions ("0.6.1" vs "0.10.0"),
// ignoring a leading v and any -prerelease suffix. It returns -1, 0 or 1, and
// ok=false when either side is not a version (a "dev" build).
func compareVersions(a, b string) (int, bool) {
	parse := func(v string) ([]int, bool) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			v = v[:i]
		}
		parts := strings.Split(v, ".")
		out := make([]int, 3)
		if len(parts) == 0 || len(parts) > 3 {
			return nil, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return nil, false
			}
			out[i] = n
		}
		return out, true
	}
	x, okA := parse(a)
	y, okB := parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range 3 {
		switch {
		case x[i] < y[i]:
			return -1, true
		case x[i] > y[i]:
			return 1, true
		}
	}
	return 0, true
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
	if cmp, ok := compareVersions(curVer, ver); ok && cmp >= 0 {
		// Equal, or this build is newer than "latest" (a pre-release or a
		// local build): never downgrade.
		fmt.Fprintf(out, "dashd is already up to date (%s; latest release %s)\n", curVer, rel.TagName)
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
	// Verify against the release's checksums.txt before anything is unpacked.
	// HTTPS protects the transfer; this protects against a truncated or
	// swapped asset, and is the same check the installers make.
	sumsURL := fmt.Sprintf("%s/%s/checksums.txt", strings.TrimRight(downloadBase, "/"), rel.TagName)
	want, err := fetchChecksum(ctx, client, sumsURL, archive)
	if err != nil {
		return err
	}
	gotSum := sha256.Sum256(raw)
	if got := hex.EncodeToString(gotSum[:]); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s, release says %s; not installing", archive, got, want)
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

// fetchChecksum returns the sha256 recorded for name in a goreleaser
// checksums.txt.
func fetchChecksum(ctx context.Context, client *http.Client, url, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "dashd")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in checksums.txt; not installing", name)
}
