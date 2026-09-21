package main

import (
	"flag"
	"net/url"
	"strings"
	"testing"
)

func TestFlagsFromEnv(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	base := fs.String("base-url", "", "")
	addr := fs.String("addr", "localhost:8080", "")
	twoFA := fs.Bool("require-2fa", false, "")
	if err := fs.Parse([]string{"-addr", ":9000"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GOPHERDEX_BASE_URL":    "https://gopherdex.dev",
		"GOPHERDEX_ADDR":        ":1", // the command line wins
		"GOPHERDEX_REQUIRE_2FA": "true",
	}
	if err := flagsFromEnv(fs, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if *base != "https://gopherdex.dev" || *addr != ":9000" || !*twoFA {
		t.Errorf("base=%q addr=%q 2fa=%v", *base, *addr, *twoFA)
	}
	env["GOPHERDEX_REQUIRE_2FA"] = "maybe"
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.Bool("require-2fa", false, "")
	if err := flagsFromEnv(fs2, func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "GOPHERDEX_REQUIRE_2FA") {
		t.Errorf("bad value: %v", err)
	}
}

func TestLaunchWarnings(t *testing.T) {
	prod, _ := url.Parse("https://gopherdex.dev")
	dev, _ := url.Parse("http://localhost:8080")
	if w := launchWarnings(dev, "", "", "", false, "localhost:8080"); len(w) != 0 {
		t.Errorf("development warnings: %v", w)
	}
	if w := launchWarnings(prod, "", "", "", false, "127.0.0.1:8080"); len(w) != 4 {
		t.Errorf("bare production config: %d warnings %v", len(w), w)
	}
	if w := launchWarnings(prod, "smtp:587", "alice", "/backups", true, "127.0.0.1:8080"); len(w) != 0 {
		t.Errorf("complete production config: %v", w)
	}
}
