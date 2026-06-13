// terraform-provider-codearmory is an OpenTofu/Terraform provider that configures
// a codearmory platform through its Conductor gateway. A single binary serves both
// OpenTofu and Terraform (they share the plugin protocol).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/code-armory-app/terraform-provider-codearmory/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// Address must match the source used in `required_providers` / dev_overrides.
		Address: "registry.terraform.io/code-armory-app/codearmory",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
