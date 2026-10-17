//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesismal/fibhub/client"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/pod"
	"github.com/lesismal/fibhub/server"
)

func runArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := run(ctx, args, &out)
	return out.String(), err
}

func TestCLIFlags(t *testing.T) {
	if out, err := runArgs(t, "-version"); err != nil || !strings.Contains(out, proto.Version) {
		t.Fatalf("-version: %q, %v", out, err)
	}
	if out, err := runArgs(t, "-help"); err != nil || !strings.Contains(out, "-mode=server|pod|client") || !strings.Contains(out, "-forward") {
		t.Fatalf("-help: %q, %v", out, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "no mode"},
		{[]string{"-mode=hub"}, "unknown mode"},
		{[]string{"server"}, "every option is a flag"},
		{[]string{"-mode=pod", "-forward=1:2=a/b"}, "-forward is not a flag of mode pod"},
		{[]string{"-mode=server", "-node=a=1:2"}, "-node is not a flag of mode server"},
		{[]string{"-mode=client", "-server=127.0.0.1:1"}, "needs a -password"},
		{[]string{"-mode=server", "-password=" + placeholder}, placeholder},
		{[]string{"-mode=server"}, "password is required"},
	} {
		if _, err := runArgs(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.args, err, tc.want)
		}
	}
}

// Each template prints, decodes into its mode's config with no field left
// over, and is refused until its passwords are set.
func TestCLITemplates(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range modes {
		out, err := runArgs(t, "-mode="+mode, "-print-config")
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(out))
		dec.DisallowUnknownFields()
		var cfg any
		switch mode {
		case "server":
			cfg = &server.Config{}
		case "pod":
			cfg = &pod.Config{}
		default:
			cfg = &client.Config{}
		}
		if err := dec.Decode(cfg); err != nil {
			t.Fatalf("%s template: %v", mode, err)
		}
		path := filepath.Join(dir, mode+".json")
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runArgs(t, "-config="+path); err == nil || !strings.Contains(err.Error(), placeholder) {
			t.Fatalf("%s template ran: %v", mode, err)
		}
	}
	if _, err := runArgs(t, "-mode=pod", "-config="+filepath.Join(dir, "server.json")); err == nil || !strings.Contains(err.Error(), "not a pod's") {
		t.Fatalf("a pod ran with a server's config: %v", err)
	}
}

// A config file's mode stands in for -mode, and flags override it.
func TestCLIConfigMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	cfg := `{"mode":"server","name":"from-file","addr":"127.0.0.1:0","webAddr":"127.0.0.1:0","password":"x"}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runArgs(t, "-config="+path, "-name=from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `fibhub server "from-flag"`) {
		t.Fatalf("output %q", out)
	}
}
