package main

import (
	"context"
	"fmt"
	"os"

	"github.com/trobrock/notch/plugin"
)

func main() {
	service := newService()
	defer service.closeEndpoint()
	if err := plugin.Serve(context.Background(), service.extension()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
