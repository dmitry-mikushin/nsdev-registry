package registry

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// session subcommand flags. Defaults match the documented port for the
// loopback TCP listener (see NEXTSILICON.md).
var (
	sessionServerURL string
	sessionProbePort int
	sessionUserFlag  string
	sessionInsecure  bool
)

func init() {
	SessionCmd.Flags().StringVar(&sessionServerURL,
		"server", "https://127.0.0.1:5000",
		"URL of the local nsdev-registry TCP/HTTPS listener "+
			"(this command must run on the same host as the registry, "+
			"reached via ssh by the operator)")
	SessionCmd.Flags().IntVar(&sessionProbePort,
		"probe-port", 0,
		"operator-side UDP port that nsdev-push has bound and "+
			"wants the registry to NAT-punch back to (required)")
	SessionCmd.Flags().StringVar(&sessionUserFlag,
		"user", "",
		"override the operator identity (defaults to $SSH_USER, then $USER)")
	SessionCmd.Flags().BoolVar(&sessionInsecure,
		"insecure", true,
		"skip TLS verification on the loopback connection "+
			"(the local TCP listener typically uses a self-signed cert)")
	RootCmd.AddCommand(SessionCmd)
}

// SessionCmd is the ssh-callable subcommand that brokers the handshake
// between an operator's nsdev-push and the QUIC listener of this
// registry. The operator invokes it through ssh:
//
//	ssh <bastion> nsdev-registry session \
//	    --server https://127.0.0.1:5000 \
//	    --probe-port 54321
//
// inside which:
//
//  1. The subcommand reads $SSH_USER (operator identity, set by sshd
//     before this process starts — not spoofable from the client side
//     without compromising the ssh server).
//  2. The subcommand reads $SSH_CLIENT (operator's source TCP address +
//     ports; the IP from there pairs with --probe-port to give the
//     operator's UDP endpoint as seen from outside the cluster).
//  3. POSTs a SessionRequest to the loopback /v3/sessions endpoint of
//     the registry on the same host. The endpoint generates a bearer
//     token, fires 10 NAT-punching probes from the QUIC listener's
//     socket at the operator's UDP endpoint, and returns a JSON
//     response with the token + the externally reachable UDP address
//     to dial.
//  4. The subcommand prints the JSON to its own stdout. nsdev-push,
//     reading the ssh stdout pipe, parses the JSON and opens its QUIC
//     connection — no ssh in the data path from here on.
var SessionCmd = &cobra.Command{
	Use:   "session",
	Short: "ssh-bridged handshake — issue a QUIC bearer token + advertise UDP endpoint",
	Long: "session is invoked by operators via ssh; it reads $SSH_USER " +
		"and $SSH_CLIENT, calls the loopback /v3/sessions endpoint of " +
		"the registry running on this host, and prints the resulting " +
		"JSON to stdout for the operator's nsdev-push to consume.",
	RunE: func(cmd *cobra.Command, args []string) error {
		user := sessionUserFlag
		if user == "" {
			user = os.Getenv("SSH_USER")
		}
		if user == "" {
			user = os.Getenv("USER")
		}
		if user == "" {
			return fmt.Errorf(
				"session: cannot determine user — pass --user or set $SSH_USER")
		}

		if sessionProbePort <= 0 || sessionProbePort > 65535 {
			return fmt.Errorf(
				"session: --probe-port must be 1..65535 (got %d)",
				sessionProbePort)
		}

		clientIP, err := operatorSourceIP()
		if err != nil {
			return fmt.Errorf("session: %w", err)
		}
		clientUDPAddr := fmt.Sprintf("%s:%d", clientIP, sessionProbePort)

		body, err := json.Marshal(map[string]any{
			"user":            user,
			"client_udp_addr": clientUDPAddr,
		})
		if err != nil {
			return err
		}

		tr := &http.Transport{}
		if sessionInsecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client := &http.Client{Transport: tr}

		url := strings.TrimRight(sessionServerURL, "/") + "/v3/sessions"
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("session: POST %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("session: registry returned %s: %s",
				resp.Status, strings.TrimSpace(string(respBody)))
		}
		// Pass the JSON through verbatim — nsdev-push parses it from
		// ssh stdout, so we must not reformat it. cmd.OutOrStdout()
		// rather than os.Stdout so tests can capture the output.
		_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
		return err
	},
}

// operatorSourceIP extracts the operator's IP from $SSH_CLIENT. sshd
// sets that env var to a 3-token string: "<client_ip> <client_port>
// <server_port>". If $SSH_CLIENT isn't set (e.g. when this subcommand
// is invoked outside an ssh session for local testing), we fall back
// to 127.0.0.1 so a local QUIC client can punch through to itself.
func operatorSourceIP() (string, error) {
	sshClient := os.Getenv("SSH_CLIENT")
	if sshClient == "" {
		// Local-loopback fallback so `nsdev-registry session
		// --probe-port N` works on the developer's own laptop for
		// smoke testing.
		return "127.0.0.1", nil
	}
	fields := strings.Fields(sshClient)
	if len(fields) < 1 {
		return "", fmt.Errorf("malformed $SSH_CLIENT=%q", sshClient)
	}
	return fields[0], nil
}
