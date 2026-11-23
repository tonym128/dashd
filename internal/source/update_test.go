package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateModelsAndReloadOpenRouter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "acme/new-test-model-99",
					"pricing": {"prompt": "0.000003", "completion": "0.000015"}
				}
			]
		}`))
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "sub", "models.json")
	n, err := UpdateModels(context.Background(), ts.URL, dest)
	if err != nil {
		t.Fatalf("UpdateModels: %v", err)
	}
	if n != 1 {
		t.Fatalf("UpdateModels count = %d, want 1", n)
	}

	pricer, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pricer.Resolve("acme/new-test-model-99"); ok {
		t.Fatal("model should not resolve before ReloadOpenRouter")
	}
	if err := pricer.ReloadOpenRouter(dest); err != nil {
		t.Fatalf("ReloadOpenRouter: %v", err)
	}
	r, ok := pricer.Resolve("acme/new-test-model-99")
	if !ok {
		t.Fatal("model did not resolve after ReloadOpenRouter")
	}
	if r.Input != 3.0 || r.Output != 15.0 {
		t.Fatalf("rates = %+v, want input=3.0 output=15.0", r)
	}
}

// The container links the dump from a directory the service user cannot write
// to; a refresh must replace the link's target, not the link.
func TestWriteModelsBytesFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "writable", "models.json")
	link := filepath.Join(dir, "lib", "models.json")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	payload := `{"data":[{"id":"acme/new"}]}`
	if _, err := WriteModelsBytes([]byte(payload), link); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link was replaced: %v, %v", fi, err)
	}
	if got, _ := os.ReadFile(target); string(got) != payload {
		t.Errorf("target = %q, want %q", got, payload)
	}
}
