package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
)

// resolveSecrets fills each token from, in order: its flag, its -*-file, its
// environment variable. A file is the recommended route: a flag value is in
// argv, which any local user can read with ps.
func resolveSecrets(o *options) error {
	for _, t := range []struct {
		val  *string
		file string
		env  string
		flag string
	}{
		{&o.authToken, o.authTokenFile, "DASHD_AUTH_TOKEN", "-auth-token-file"},
		{&o.ingestToken, o.ingestTokenFile, "DASHD_INGEST_TOKEN", "-ingest-token-file"},
		{&o.adminToken, o.adminTokenFile, "DASHD_ADMIN_TOKEN", "-admin-token-file"},
	} {
		if *t.val != "" {
			continue
		}
		if t.file != "" {
			v, err := readSecretFile(t.file)
			if err != nil {
				return fmt.Errorf("%s: %w", t.flag, err)
			}
			*t.val = v
			continue
		}
		*t.val = os.Getenv(t.env)
	}
	return nil
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Unix permission bits only; Windows reports 0666 for any writable file and
	// protects the file with its ACL instead.
	if fi, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		slog.Default().Warn("secret file is readable by other users; chmod 600 it", "path", path,
			"mode", fmt.Sprintf("%04o", fi.Mode().Perm()))
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

// loadMachineTokens reads `machine token` lines (# comments allowed). Each
// token may only push as its machine.
func loadMachineTokens(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-machine-tokens-file: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		slog.Default().Warn("machine tokens file is readable by other users; chmod 600 it", "path", path)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want `machine token`", path, line)
		}
		if len(fields[1]) < 16 {
			return nil, fmt.Errorf("%s:%d: token for %s is shorter than 16 characters", path, line, fields[0])
		}
		if _, dup := out[fields[1]]; dup {
			return nil, fmt.Errorf("%s:%d: token reused for a second machine", path, line)
		}
		out[fields[1]] = fields[0]
	}
	return out, sc.Err()
}

// warnInsecureRemote is loud about sending a bearer token in clear text.
func warnInsecureRemote(remote string, log *slog.Logger) {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	log.Warn("pushing over plain HTTP: the token and every session are sent unencrypted",
		"remote", remote, "fix", "put TLS in front of the server (Caddy, Tailscale) or use -tls-cert/-tls-key on it")
}
