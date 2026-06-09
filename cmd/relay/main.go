// Command relay is the InternetMerge Phase 3 bonding relay. Run it on a VPS the
// user controls; the desktop app connects K NIC-bound flows to it and it
// reassembles a single stream, dials the upstream, and stripes the response back.
package main

import (
	"encoding/base64"
	"flag"
	"log"
	"net"
	"os"

	"github.com/kaandikec/internetmerge/internal/relay"
	"github.com/kaandikec/internetmerge/internal/version"
)

func main() {
	listen := flag.String("listen", ":7000", "TCP address to listen on")
	keyB64 := flag.String("key", "", "base64-encoded shared key (or set INTERNETMERGE_RELAY_KEY)")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVer {
		log.Printf("internetmerge-relay %s", version.Version)
		return
	}
	k := *keyB64
	if k == "" {
		k = os.Getenv("INTERNETMERGE_RELAY_KEY")
	}
	if k == "" {
		log.Fatal("relay: no key provided (-key or INTERNETMERGE_RELAY_KEY)")
	}
	key, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(key) < 16 {
		log.Fatalf("relay: invalid key: %v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("relay: listen %s: %v", *listen, err)
	}
	log.Printf("internetmerge-relay %s listening on %s", version.Version, *listen)
	log.Fatal(relay.New(key).Serve(ln))
}
