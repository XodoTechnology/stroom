// Stroom — a lightweight torrent + NZB download client.
// Single static binary. Web UI served from memory. No Electron, no bloat.
package main

import (
	"fmt"
	"os"
)

var version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `stroom — lightweight torrent + NZB client

usage:
  stroom [serve] [flags]     start the client + web UI (default command)
  stroom add <uri|file>      add a torrent/NZB/magnet to a running instance
  stroom gui                 same as serve (native window by default)
  stroom create <path>       create a .torrent file from a file or directory
  stroom update [--check]    check for / apply updates from GitHub releases
  stroom version             print version

flags for serve:
  -addr string   UI listen address (overrides config)
  -data string   data directory (overrides $STROOM_DATA / ~/.stroom)
  -dir string    download directory (overrides config)
`)
}

func main() {
	attachParentConsole() // windows: restore stdio when run from a terminal
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "add":
		err = runAdd(args)
	case "gui":
		err = runServe(append(args, "-gui"))
	case "create":
		err = runCreateTorrent(args)
	case "update":
		err = runUpdate(args)
	case "version", "-v", "--version":
		fmt.Println("stroom", version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stroom:", err)
		os.Exit(1)
	}
}
