// Package tailnet joins the server to a Tailscale network as a node of its
// own, using tsnet's userspace networking, so the server needs no tailscaled
// on the host.  It provides an HTTPS listener on the node's MagicDNS name
// and the identity lookup the auth package needs.
package tailnet

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/rjbs/taskmaster/internal/auth"
)

type Node struct {
	srv *tsnet.Server
	lc  *local.Client

	// DNSName is the node's MagicDNS name, like "taskmaster.tail0324d.ts.net".
	DNSName string
}

// Start brings the node up and waits until it is connected.  The first time
// a node runs it must be authorized: tsnet looks for an auth key in the
// TS_AUTHKEY environment variable and otherwise logs a login URL to visit,
// through logger.  State is kept in dir so later starts need no
// authorization.
func Start(ctx context.Context, hostname, dir string, logger *slog.Logger) (*Node, error) {
	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      dir,
		UserLogf: func(format string, args ...any) {
			logger.Info("tailscale: " + fmt.Sprintf(format, args...))
		},
		Logf: func(format string, args ...any) {
			logger.Debug("tailscale: " + fmt.Sprintf(format, args...))
		},
	}
	status, err := srv.Up(ctx)
	if err != nil {
		srv.Close()
		return nil, err
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, err
	}
	n := &Node{srv: srv, lc: lc}
	if status.Self != nil {
		n.DNSName = trimDot(status.Self.DNSName)
	}
	return n, nil
}

// Listen listens on the node: with https, for HTTPS on port 443 with a
// certificate Tailscale obtains for the node's MagicDNS name, which needs
// HTTPS certificates enabled in the tailnet admin console; otherwise for
// plain HTTP on port 80.
func (n *Node) Listen(https bool) (net.Listener, error) {
	if https {
		return n.srv.ListenTLS("tcp", ":443")
	}
	return n.srv.Listen("tcp", ":80")
}

// URL is the base URL clients should use for the node.
func (n *Node) URL(https bool) string {
	if https {
		return "https://" + n.DNSName + "/"
	}
	return "http://" + n.DNSName + "/"
}

// WhoIs resolves a connection's remote address to the Tailscale identity
// behind it.
func (n *Node) WhoIs(ctx context.Context, remoteAddr string) (auth.Identity, error) {
	who, err := n.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return auth.Identity{}, err
	}
	id := auth.Identity{Tagged: who.Node != nil && who.Node.IsTagged()}
	if who.UserProfile != nil {
		id.Login = who.UserProfile.LoginName
	}
	return id, nil
}

func (n *Node) Close() error {
	return n.srv.Close()
}

func trimDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}
