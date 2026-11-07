// Package config reads the server's configuration file.
//
// The file is JSON.  A complete example:
//
//	{
//	  "listen": "127.0.0.1:8080",
//	  "baseUrl": "",
//	  "timeZone": "America/New_York",
//	  "dataDir": "data",
//	  "tailscale": { "hostname": "taskmaster", "dir": "tailscale", "https": true },
//	  "demoClient": true,
//	  "users": {
//	    "rjbs": {
//	      "credentials": [
//	        { "type": "loopback" },
//	        { "type": "tailscale", "login": "rjbs@github" },
//	        { "type": "tailscale", "login": "rjbs-work@github", "accountIds": [ "work" ] },
//	        { "type": "bearer", "tokenHash": "<from taskmaster new-token>" }
//	      ],
//	      "primaryAccount": "rjbs"
//	    }
//	  },
//	  "accounts": {
//	    "rjbs": { "owner": "rjbs", "name": "Personal" },
//	    "work": { "owner": "rjbs", "name": "Work", "path": "/srv/tm/work.sqlite" }
//	  }
//	}
//
// Relative paths are resolved against the directory holding the file.  An
// account's database defaults to <dataDir>/<account id>.sqlite.  A
// credential with accountIds sees only those accounts.  primaryAccount is
// optional; a user who owns several accounts and names none has no
// primary account.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/rjbs/taskmaster/internal/auth"
)

type File struct {
	// Listen is the address of the plain HTTP listener, whose requests
	// act as the loopback user.  Empty disables it.
	Listen string `json:"listen"`

	// BaseURL, if set, is used for the absolute URLs in the Session
	// object instead of deriving them from each request.
	BaseURL string `json:"baseUrl"`

	// TimeZone is the server's fallback zone for tasks without one.
	TimeZone string `json:"timeZone"`

	// DataDir holds account databases that do not name their own path.
	DataDir string `json:"dataDir"`

	// Tailscale, if present, makes the server join the tailnet as its own
	// node and serve HTTPS there.
	Tailscale *Tailscale `json:"tailscale"`

	// DemoClient, which defaults to true, serves the TaskBlaster page at
	// the root of every listener.
	DemoClient *bool `json:"demoClient"`

	Users    map[string]*User    `json:"users"`
	Accounts map[string]*Account `json:"accounts"`
}

type Tailscale struct {
	// Hostname is the node name, which becomes <hostname>.<tailnet>.ts.net.
	Hostname string `json:"hostname"`

	// Dir holds the node's state (keys and the like) between runs.
	Dir string `json:"dir"`

	// HTTPS, which defaults to true, serves TLS on port 443 with a
	// certificate from Tailscale; that needs HTTPS certificates enabled
	// in the tailnet admin console.  False serves plain HTTP on port 80,
	// which the tailnet still encrypts on the wire.
	HTTPS *bool `json:"https"`
}

type User struct {
	Credentials    []Credential `json:"credentials"`
	PrimaryAccount string       `json:"primaryAccount"`
}

type Credential struct {
	Type       string   `json:"type"`
	Login      string   `json:"login"`
	TokenHash  string   `json:"tokenHash"`
	AccountIDs []string `json:"accountIds"`
}

type Account struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

// Config is a loaded, validated configuration.
type Config struct {
	File
	Location  *time.Location
	Directory *auth.Directory
}

// Account ids become file names, and users ids appear in the Session, so
// both are kept to a boring alphabet.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Load reads and validates the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(raw, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse validates configuration text, resolving relative paths against
// baseDir.
func Parse(raw []byte, baseDir string) (*Config, error) {
	cfg := &Config{File: File{
		Listen:   "127.0.0.1:8080",
		TimeZone: "America/New_York",
		DataDir:  ".",
	}}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg.File); err != nil {
		return nil, err
	}

	loc, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("timeZone: %w", err)
	}
	cfg.Location = loc

	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	cfg.DataDir = resolve(cfg.DataDir)
	if cfg.Tailscale != nil {
		if cfg.Tailscale.Hostname == "" {
			return nil, fmt.Errorf("tailscale.hostname is required")
		}
		if cfg.Tailscale.Dir == "" {
			cfg.Tailscale.Dir = filepath.Join(cfg.DataDir, "tailscale")
		}
		cfg.Tailscale.Dir = resolve(cfg.Tailscale.Dir)
		if cfg.Tailscale.HTTPS == nil {
			t := true
			cfg.Tailscale.HTTPS = &t
		}
	}

	if cfg.DemoClient == nil {
		t := true
		cfg.DemoClient = &t
	}

	users := map[string]*auth.User{}
	for id, u := range cfg.Users {
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("user id %q is not allowed", id)
		}
		au := &auth.User{PrimaryAccount: u.PrimaryAccount}
		for _, c := range u.Credentials {
			au.Credentials = append(au.Credentials, auth.Credential{Type: c.Type, Login: c.Login, TokenHash: c.TokenHash, AccountIDs: c.AccountIDs})
		}
		users[id] = au
	}
	accounts := map[string]*auth.Account{}
	for id, a := range cfg.Accounts {
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("account id %q is not allowed", id)
		}
		path := a.Path
		if path == "" {
			path = filepath.Join(cfg.DataDir, id+".sqlite")
		}
		name := a.Name
		if name == "" {
			name = id
		}
		accounts[id] = &auth.Account{Name: name, Owner: a.Owner, Path: resolve(path)}
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("at least one account is required")
	}
	dir, err := auth.NewDirectory(users, accounts)
	if err != nil {
		return nil, err
	}
	cfg.Directory = dir
	return cfg, nil
}
