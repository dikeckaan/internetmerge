package main

import (
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/kaandikec/internetmerge/internal/relay"
)

// runRelay implements `internetmerge relay`: run the bonding relay server, or
// with --keygen mint a key and print a connection string.
func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", ":7000", "TCP address to listen on")
	keyFlag := fs.String("key", "", "base64 shared key (or set INTERNETMERGE_RELAY_KEY)")
	keygen := fs.Bool("keygen", false, "generate a new key + connection string, then exit")
	fs.Parse(args)

	if *keygen {
		k, err := relay.GenerateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		port := portOf(*listen)
		fmt.Println("Relay key generated. Paste this into InternetMerge:")
		fmt.Printf("  Address: %s:%s\n", "YOUR_SERVER_IP", port)
		fmt.Printf("  Key:     %s\n", k)
		fmt.Println()
		fmt.Println("Start the relay with:")
		fmt.Printf("  INTERNETMERGE_RELAY_KEY=%s internetmerge relay --listen %s\n", k, *listen)
		return
	}

	key := *keyFlag
	if key == "" {
		key = os.Getenv("INTERNETMERGE_RELAY_KEY")
	}
	if err := relay.ListenAndServe(*listen, key, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// portOf extracts the port from a listen address like ":7000" or "0.0.0.0:7000".
// Falls back to "7000" when the address has no parseable port.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "7000"
}
