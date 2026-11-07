// Package dashboard serves the token-gated, static Lambda console.
package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	stdlog "log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed index.html app.js
var files embed.FS

// Handler checks the query token before returning any page bytes. The browser
// calls the API directly so this VM can sleep during background builds.
func handler(expected [32]byte, apiOrigin string) (http.Handler, error) {
	u, err := url.Parse(apiOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("LAMBDA_CONTROLPLANE_URL must be an HTTPS origin")
	}
	apiOrigin = "https://" + u.Host
	template, err := files.ReadFile("index.html")
	if err != nil {
		return nil, err
	}
	script, err := files.ReadFile("app.js")
	if err != nil {
		return nil, err
	}
	page := bytes.ReplaceAll(template, []byte("__API_ORIGIN__"), []byte(html.EscapeString(apiOrigin)))
	page = bytes.ReplaceAll(page, []byte("__SCRIPT__"), script)
	scriptHash := sha256.Sum256(script)
	csp := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(scriptHash[:]) + "'; connect-src " + apiOrigin + "; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	valid := func(candidate string) bool {
		hash := sha256.Sum256([]byte(candidate))
		return subtle.ConstantTimeCompare(hash[:], expected[:]) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp)
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			values := r.Header.Values("Authorization")
			if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || !valid(strings.TrimPrefix(values[0], "Bearer ")) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		values := query["token"]
		if err != nil || len(values) != 1 || !valid(values[0]) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(page)
		}
	}), nil
}

func Run(ctx context.Context, log io.Writer) error {
	hash, err := hex.DecodeString(os.Getenv("LAMBDA_ACCESS_TOKEN_SHA256"))
	if err != nil || len(hash) != 32 {
		return errors.New("LAMBDA_ACCESS_TOKEN_SHA256 must be a SHA-256 token digest")
	}
	var expected [32]byte
	copy(expected[:], hash)
	handler, err := handler(expected, os.Getenv("LAMBDA_CONTROLPLANE_URL"))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: stdlog.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	fmt.Fprintln(log, "Lambda dashboard listening on :8080") // Never log request URLs.
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
