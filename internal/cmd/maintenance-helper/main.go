package main

import (
	"context"
	"os"
	"time"

	"github.com/henrygd/beszel/internal/maintenance"
)

func main() {
	if os.Geteuid() != 0 {
		os.Exit(77)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	if err := maintenance.NewHelper().Serve(ctx, os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
