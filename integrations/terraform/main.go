package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	provider "github.com/infercrane/infercrane/integrations/terraform/internal/provider"
)

var version = "dev"

func main() {
	err := providerserver.Serve(context.Background(), func() provider.ProviderFactory { return provider.New(version) }, providerserver.ServeOpts{Address: "registry.terraform.io/infercrane/infercrane"})
	if err != nil {
		log.Fatal(err)
	}
}
