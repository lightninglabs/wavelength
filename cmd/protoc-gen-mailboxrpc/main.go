package main

import (
	"flag"

	"github.com/lightninglabs/darepo-client/cmd/protoc-gen-mailboxrpc/internal/gen"
	"google.golang.org/protobuf/compiler/protogen"
)

// main runs the protoc plugin entrypoint.
func main() {
	var flags flag.FlagSet

	excludeService := flags.String("exclude_service", "",
		"fully-qualified proto service to skip (repeat not supported)",
	)

	opts := protogen.Options{
		ParamFunc: flags.Set,
	}

	opts.Run(func(plugin *protogen.Plugin) error {
		cfg := gen.Config{
			ExcludeService: *excludeService,
		}

		return gen.Generate(plugin, cfg)
	})
}
