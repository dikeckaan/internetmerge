// Command internetmerge-gui is the desktop (Wails) entrypoint for InternetMerge.
// It presents the connection-bonding engine with a UI for selecting interfaces,
// starting/stopping bonding, and watching live per-link throughput.
package main

import (
	"embed"
	"fmt"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"

	"github.com/kaandikec/internetmerge/internal/version"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Lets us confirm the exact runtime version baked into a GUI build.
	if len(os.Args) > 1 && (os.Args[1] == "--print-version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
		return
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "InternetMerge",
		Width:     920,
		Height:    680,
		MinWidth:  720,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []interface{}{app},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
			About: &mac.AboutInfo{
				Title:   "InternetMerge",
				Message: "Bond multiple network links into faster total internet.",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
