package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/scan"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

func runDoctor(ctx context.Context, o options, out io.Writer) error {
	fmt.Fprintf(out, "dashd doctor (%s)\n", version)
	fmt.Fprintf(out, "machine:  %s\n", o.machine)
	fmt.Fprintf(out, "home:     %s\n", o.home)
	fmt.Fprintf(out, "db:       %s\n", o.db)

	if st, err := store.Open(o.db); err != nil {
		fmt.Fprintf(out, "db check: FAIL (%v)\n", err)
	} else {
		defer st.Close()
		tot, err := st.Totals(ctx, store.Filter{})
		if err != nil {
			fmt.Fprintf(out, "db check: FAIL (%v)\n", err)
		} else {
			fmt.Fprintf(out, "db check: ok (%d sessions, %d calls)\n", tot.Sessions, tot.Calls)
		}
	}

	if info, err := os.Stat(o.models); err != nil {
		fmt.Fprintf(out, "models:   missing at %s (fallback table only)\n", o.models)
	} else {
		fmt.Fprintf(out, "models:   %s (%d bytes, updated %s)\n",
			o.models, info.Size(), info.ModTime().Format("2006-01-02 15:04"))
	}

	sources, openCodeDB := scan.DefaultSources(o.home)
	if o.opencode != "" {
		openCodeDB = o.opencode
	}
	fmt.Fprintln(out, "sources:")
	for _, src := range sources {
		if _, err := os.Stat(src.Root); err == nil {
			fmt.Fprintf(out, "  %-10s found (%s)\n", src.Agent, src.Root)
		} else {
			fmt.Fprintf(out, "  %-10s not found (%s)\n", src.Agent, src.Root)
		}
	}
	if _, err := os.Stat(openCodeDB); err == nil {
		fmt.Fprintf(out, "  %-10s found (%s)\n", "opencode", openCodeDB)
	} else {
		fmt.Fprintf(out, "  %-10s not found (%s)\n", "opencode", openCodeDB)
	}

	if o.remote != "" {
		base := strings.TrimRight(o.remote, "/")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			fmt.Fprintf(out, "remote:   FAIL %s/healthz (%v)\n", base, err)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			fmt.Fprintf(out, "remote:   %s/healthz -> %d %s\n", base, resp.StatusCode, strings.TrimSpace(string(body)))
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, base+"/api/models", nil)
		tok := o.ingestToken
		if tok == "" {
			tok = o.authToken
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if mresp, err := client.Do(req); err != nil {
			fmt.Fprintf(out, "auth:     FAIL %s/api/models (%v)\n", base, err)
		} else {
			mresp.Body.Close()
			fmt.Fprintf(out, "auth:     %s/api/models -> %d\n", base, mresp.StatusCode)
		}
	}

	if p, err := source.NewPricer(o.models, ""); err == nil {
		_ = p
	}
	return nil
}
