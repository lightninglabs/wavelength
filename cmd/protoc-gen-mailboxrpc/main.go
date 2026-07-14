package main

import (
	"flag"

	"github.com/lightninglabs/wavelength/cmd/protoc-gen-mailboxrpc/internal/gen"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

// main runs the protoc plugin entrypoint.
func main() {
	var flags flag.FlagSet

	excludeService := flags.String(
		"exclude_service", "",
		"fully-qualified proto service to skip (repeat not supported)",
	)

	opts := protogen.Options{
		ParamFunc: flags.Set,
	}

	opts.Run(func(plugin *protogen.Plugin) error {
		plugin.SupportedFeatures = uint64(
			pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL,
		)

		cfg := gen.Config{
			ExcludeService: *excludeService,
		}

		return gen.Generate(plugin, cfg)
	})
}
