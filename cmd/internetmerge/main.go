// Command internetmerge is the command-line entrypoint for the InternetMerge
// connection-bonding core. It drives the same engine the GUI uses, so the stack
// can be exercised and tested headlessly.
//
// Usage:
//
//	internetmerge list
//	    Print discovered network interfaces.
//
//	internetmerge run --interfaces en0,en7 [--addr 127.0.0.1:1080] [--set-proxy Wi-Fi]
//	    Start the local SOCKS5 dispatcher bonding the given interfaces. With
//	    --set-proxy, point those macOS network services at the proxy and restore
//	    them on exit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kaandikec/internetmerge/internal/engine"
	"github.com/kaandikec/internetmerge/internal/netif"
	"github.com/kaandikec/internetmerge/internal/sysproxy"
	"github.com/kaandikec/internetmerge/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list":
		runList()
	case "run":
		runServe(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("InternetMerge", version.Version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `InternetMerge — bond multiple network links into faster total internet.

Commands:
  list                       List network interfaces
  run --interfaces a,b ...   Start the SOCKS5 dispatcher

Run "internetmerge run -h" for run flags.
`)
}

func runList() {
	ifaces, err := netif.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("%-8s %-24s %-16s %-6s\n", "DEVICE", "LABEL", "IPv4", "USABLE")
	for _, it := range ifaces {
		fmt.Printf("%-8s %-24s %-16s %-6t\n", it.Name, it.Label, dash(it.IPv4), it.Usable())
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	ifList := fs.String("interfaces", "", "comma-separated interface names to bond, e.g. en0,en7")
	auto := fs.Bool("auto", false, "auto-select every usable interface (and, with --auto-proxy, all network services)")
	autoProxy := fs.Bool("auto-proxy", false, "with --auto, also route all detected network services through the proxy")
	addr := fs.String("addr", "127.0.0.1:1080", "local SOCKS5 listen address")
	setProxy := fs.String("set-proxy", "", "comma-separated network services to point at the proxy (e.g. Wi-Fi); restored on exit")
	fs.Parse(args)

	interfaces := splitCSV(*ifList)
	services := splitCSV(*setProxy)
	if *auto {
		auto, err := netif.UsableNames()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		interfaces = auto
		if *autoProxy {
			if svcs, err := sysproxy.Services(); err == nil {
				services = svcs
			}
		}
	}
	if len(interfaces) == 0 {
		fmt.Fprintln(os.Stderr, "error: no interfaces (use --interfaces a,b or --auto; see: internetmerge list)")
		os.Exit(2)
	}

	eng := engine.New()
	if err := eng.Start(engine.Config{
		Interfaces:    interfaces,
		Addr:          *addr,
		ProxyServices: services,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer eng.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := eng.Status()
	fmt.Printf("InternetMerge running. Bonding %s via SOCKS5 %s\n", strings.Join(interfaces, ", "), st.Addr)
	if len(st.ProxyServices) > 0 {
		fmt.Printf("System SOCKS proxy enabled for: %s\n", strings.Join(st.ProxyServices, ", "))
	} else {
		fmt.Printf("Tip: configure apps to use SOCKS5 %s, or re-run with --set-proxy.\n", st.Addr)
	}
	fmt.Println("Press Ctrl-C to stop.")

	printStats(ctx, eng)
}

// printStats prints per-interface throughput every 2s until ctx is cancelled.
func printStats(ctx context.Context, eng *engine.Engine) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	interactive := isTTY()

	type prevSample struct{ up, down uint64 }
	prev := map[string]prevSample{}
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nShutting down...")
			return
		case <-ticker.C:
			st := eng.Status()
			var b strings.Builder
			for _, l := range st.Links {
				p := prev[l.IfName]
				prev[l.IfName] = prevSample{up: l.BytesUp, down: l.BytesDown}
				name := l.Label
				if name == "" || name == l.IfName {
					name = l.IfName
				} else {
					name = fmt.Sprintf("%s (%s)", l.Label, l.IfName)
				}
				fmt.Fprintf(&b, "  %-20s w=%-2d alive=%-5t conns=%-3d down %s/s  up %s/s\n",
					name, l.Weight, l.Alive, l.Connections,
					humanRate((l.BytesDown-p.down)/2), humanRate((l.BytesUp-p.up)/2))
			}
			if b.Len() == 0 {
				continue
			}
			if interactive {
				fmt.Print("\033[2J\033[H")
			}
			fmt.Println("InternetMerge live stats:")
			fmt.Print(b.String())
		}
	}
}

// --- small helpers ---

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanRate(bytesPerSec uint64) string {
	const unit = 1024
	if bytesPerSec < unit {
		return fmt.Sprintf("%dB", bytesPerSec)
	}
	div, exp := uint64(unit), 0
	for n := bytesPerSec / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(bytesPerSec)/float64(div), "KMGT"[exp])
}
