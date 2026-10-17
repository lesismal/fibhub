// Package common holds what fibhub's three modes share around their web
// pages: JSON answers and requests, an optional password on the page, and
// the JSON files their settings are saved in.
package common

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"os"
	"path/filepath"
	"strings"

	fibhttp "github.com/lesismal/fib/http"
)

// JSON answers with v as JSON.
func JSON(c *fibhttp.Context, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		Error(c, stdhttp.StatusInternalServerError, err)
		return
	}
	_ = c.Respond(status, "application/json; charset=utf-8", b)
}

// OK answers with {"ok":true}.
func OK(c *fibhttp.Context) { JSON(c, stdhttp.StatusOK, map[string]bool{"ok": true}) }

// Error answers with {"error": err}.
func Error(c *fibhttp.Context, status int, err error) {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	_ = c.Respond(status, "application/json; charset=utf-8", b)
}

// Bind decodes the request's JSON body into v.
func Bind(c *fibhttp.Context, v any) error {
	body := c.Body()
	if len(body) == 0 {
		return errors.New("empty request body")
	}
	return json.Unmarshal(body, v)
}

// Async runs fn on a goroutine of its own, for a handler that waits on the
// network, holding the request until fn is done.
func Async(c *fibhttp.Context, fn func()) { go c.Async(fn)() }

//go:embed static
var static embed.FS

// Static serves the styles and scripts the pages share, under /static/.
func Static(r *fibhttp.Router) {
	r.Get("/static/{file}", func(c *fibhttp.Context) {
		name := c.Param("file")
		b, err := static.ReadFile("static/" + name)
		if err != nil {
			_ = c.Respond(stdhttp.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
			return
		}
		typ := "application/octet-stream"
		switch {
		case strings.HasSuffix(name, ".css"):
			typ = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			typ = "text/javascript; charset=utf-8"
		}
		_ = c.Respond(stdhttp.StatusOK, typ, b)
	})
}

// Page serves html.
func Page(html []byte) fibhttp.HandlerFunc {
	return func(c *fibhttp.Context) { _ = c.Respond(stdhttp.StatusOK, "text/html; charset=utf-8", html) }
}

// BasicAuth asks for user:password, given as "user:password", on every
// request; an empty auth asks for nothing.
func BasicAuth(auth string) func(fibhttp.Handler) fibhttp.Handler {
	user, pass, _ := strings.Cut(auth, ":")
	return func(next fibhttp.Handler) fibhttp.Handler {
		if auth == "" {
			return next
		}
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			u, p, ok := c.Request.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				_ = c.WriteResponse(fibhttp.Response{
					StatusCode: stdhttp.StatusUnauthorized,
					Header:     stdhttp.Header{"Www-Authenticate": {`Basic realm="fibhub"`}},
					Body:       []byte("unauthorized\n"),
				})
				return
			}
			next.ServeHTTP(c)
		})
	}
}

// LoadJSON decodes the file at path into v. A path that does not exist
// leaves v as it is.
func LoadJSON(path string, v any) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// SaveJSON writes v to the file at path, replacing it whole; an empty path
// saves nothing.
func SaveJSON(path string, v any) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fibhub-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// StringList is a flag that may be given many times.
type StringList []string

func (l *StringList) String() string { return strings.Join(*l, ",") }

func (l *StringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}
