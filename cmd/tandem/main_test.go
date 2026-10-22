package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRemoteBindRequiresExplicitStrongAuthenticationAndTLS(t *testing.T) {
	for _, tc := range []struct {
		bind             string
		remote           bool
		token, cert, key string
		proxy            bool
		want             bool
	}{
		{bind: "127.0.0.1:7433", want: true},
		{bind: "[::1]:7433", want: true},
		{bind: "0.0.0.0:7433"},
		{bind: ":7433"},
		{bind: "0.0.0.0:7433", remote: true, token: "too-short", proxy: true},
		{bind: "0.0.0.0:7433", remote: true, token: strings.Repeat("a", 32)},
		{bind: "0.0.0.0:7433", remote: true, token: strings.Repeat("a", 32), proxy: true, want: true},
		{bind: "0.0.0.0:7433", remote: true, token: strings.Repeat("a", 32), cert: "cert", key: "key", want: true},
		{bind: "127.0.0.1:7433", cert: "cert"},
	} {
		err := validateBind(tc.bind, tc.remote, tc.token, tc.cert, tc.key, tc.proxy)
		if (err == nil) != tc.want {
			t.Fatalf("bind %s remote %v: %v", tc.bind, tc.remote, err)
		}
	}
}

func TestLocalVersionDoesNotConnectToService(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "0.1.0" {
		t.Fatal("unexpected local version")
	}
}

func TestServeRejectsNativeAgentOverride(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"serve", "--agent", "build"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -agent") {
		t.Fatalf("unsupported native agent override was not rejected during argument parsing: %v", err)
	}
}
