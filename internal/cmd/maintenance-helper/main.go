package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/henrygd/beszel/internal/buildinfo"
	"github.com/henrygd/beszel/internal/maintenance"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("%s\nprotocol %d\n", buildinfo.VersionLine("Maintenance Helper"), maintenance.ProtocolVersion())
		return
	}
	if os.Geteuid() != 0 {
		os.Exit(77)
	}
	// Convert an early peer close into a regular EPIPE returned by Encode so
	// systemd does not report the one-shot unit as killed by SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	if err := maintenance.NewHelper().Serve(ctx, os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
