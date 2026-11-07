package config

import (
	"slices"
	"strings"
	"testing"
)

func checkParse(t *testing.T, name, text, wantErr string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(text), "/base")
	switch {
	case wantErr == "" && err != nil:
		t.Errorf("%s: unexpected error: %v", name, err)
	case wantErr != "" && err == nil:
		t.Errorf("%s: expected error containing %q, got none", name, wantErr)
	case wantErr != "" && !strings.Contains(err.Error(), wantErr):
		t.Errorf("%s: error %q does not contain %q", name, err, wantErr)
	}
	return cfg
}

func TestParse(t *testing.T) {
	cfg := checkParse(t, "minimal", `{
		"users":    { "rjbs": { "credentials": [ { "type": "loopback" } ] } },
		"accounts": { "rjbs": { "owner": "rjbs" } }
	}`, "")
	if cfg != nil {
		if got := cfg.Directory.Accounts["rjbs"].Path; got != "/base/rjbs.sqlite" {
			t.Errorf("default path: got %q", got)
		}
		if got := cfg.Directory.Users["rjbs"].PrimaryAccount; got != "rjbs" {
			t.Errorf("implied primary: got %q", got)
		}
		if cfg.Directory.LoopbackUser() == nil {
			t.Error("loopback user not indexed")
		}
		if !*cfg.DemoClient {
			t.Error("demo client should default to on")
		}
	}

	cfg = checkParse(t, "no demo client", `{
		"demoClient": false,
		"users":    { "rjbs": { "credentials": [ { "type": "loopback" } ] } },
		"accounts": { "rjbs": { "owner": "rjbs" } }
	}`, "")
	if cfg != nil && *cfg.DemoClient {
		t.Error("demoClient: false was ignored")
	}

	cfg = checkParse(t, "two accounts", `{
		"dataDir": "data",
		"users": { "rjbs": {
			"credentials": [ { "type": "tailscale", "login": "rjbs@github" } ],
			"primaryAccount": "work"
		} },
		"accounts": {
			"rjbs": { "owner": "rjbs", "name": "Personal" },
			"work": { "owner": "rjbs", "path": "/elsewhere/work.sqlite" }
		}
	}`, "")
	if cfg != nil {
		if got := cfg.Directory.Accounts["rjbs"].Path; got != "/base/data/rjbs.sqlite" {
			t.Errorf("dataDir path: got %q", got)
		}
		if got := cfg.Directory.Accounts["work"].Path; got != "/elsewhere/work.sqlite" {
			t.Errorf("explicit path: got %q", got)
		}
		if got := cfg.Directory.Users["rjbs"].Accounts; len(got) != 2 {
			t.Errorf("accounts: got %v", got)
		}
		if cfg.Directory.TailscaleUser("rjbs@github") == nil {
			t.Error("tailscale login not indexed")
		}
		if cfg.Directory.LoopbackUser() != nil {
			t.Error("unexpected loopback user")
		}
	}

	checkParse(t, "unknown field", `{ "lisen": "x", "users": {}, "accounts": {} }`, "unknown field")
	checkParse(t, "no accounts", `{ "users": { "rjbs": {} }, "accounts": {} }`, "at least one account")
	checkParse(t, "unknown owner", `{ "users": {}, "accounts": { "a": { "owner": "nobody" } } }`, "unknown user")
	cfg = checkParse(t, "no primary", `{
		"users": { "rjbs": {} },
		"accounts": { "a": { "owner": "rjbs" }, "b": { "owner": "rjbs" } }
	}`, "")
	if cfg != nil {
		if got := cfg.Directory.Users["rjbs"].PrimaryAccount; got != "" {
			t.Errorf("several accounts should imply no primary: got %q", got)
		}
	}
	checkParse(t, "bad primary", `{
		"users": { "rjbs": { "primaryAccount": "zzz" } },
		"accounts": { "a": { "owner": "rjbs" } }
	}`, "does not own")
	cfg = checkParse(t, "limited credential", `{
		"users": { "rjbs": { "credentials": [
			{ "type": "loopback" },
			{ "type": "tailscale", "login": "work@github", "accountIds": [ "work", "side" ] }
		] } },
		"accounts": { "rjbs": { "owner": "rjbs" }, "work": { "owner": "rjbs" }, "side": { "owner": "rjbs" } }
	}`, "")
	if cfg != nil {
		if got := cfg.Directory.LoopbackUser().Accounts; len(got) != 3 {
			t.Errorf("unlimited credential accounts: got %v", got)
		}
		if got := cfg.Directory.TailscaleUser("work@github").Accounts; !slices.Equal(got, []string{"side", "work"}) {
			t.Errorf("limited credential accounts: got %v", got)
		}
	}
	checkParse(t, "credential for unowned account", `{
		"users": {
			"a": { "credentials": [ { "type": "loopback", "accountIds": [ "a", "b" ] } ] },
			"b": {}
		},
		"accounts": { "a": { "owner": "a" }, "b": { "owner": "b" } }
	}`, "does not own")
	checkParse(t, "one login twice", `{
		"users": { "a": { "credentials": [
			{ "type": "tailscale", "login": "a@github" },
			{ "type": "tailscale", "login": "a@github", "accountIds": [ "a" ] }
		] } },
		"accounts": { "a": { "owner": "a" } }
	}`, "claims tailscale login \"a@github\" twice")
	checkParse(t, "empty accountIds", `{
		"users": { "a": { "credentials": [ { "type": "loopback", "accountIds": [] } ] } },
		"accounts": { "a": { "owner": "a" } }
	}`, "empty accountIds")
	checkParse(t, "two loopbacks", `{
		"users": {
			"a": { "credentials": [ { "type": "loopback" } ] },
			"b": { "credentials": [ { "type": "loopback" } ] }
		},
		"accounts": { "a": { "owner": "a" }, "b": { "owner": "b" } }
	}`, "both claim the loopback")
	checkParse(t, "bad credential", `{
		"users": { "a": { "credentials": [ { "type": "passkey" } ] } },
		"accounts": { "a": { "owner": "a" } }
	}`, "unknown type")
	checkParse(t, "bad id", `{ "users": { "a": {} }, "accounts": { "../x": { "owner": "a" } } }`, "not allowed")
	checkParse(t, "bad zone", `{ "timeZone": "Mars/Olympus", "users": { "a": {} }, "accounts": { "a": { "owner": "a" } } }`, "timeZone")
	checkParse(t, "tailscale needs hostname", `{ "tailscale": {}, "users": { "a": {} }, "accounts": { "a": { "owner": "a" } } }`, "hostname")
}
