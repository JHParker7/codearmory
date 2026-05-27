package main

import (
	_ "embed"

	"codearmory.local/svckit/registry"
)

//go:embed service.json
var serviceConfigData []byte

var serviceConfig registry.ServiceConfig

func init() {
	var err error
	serviceConfig, err = registry.LoadConfig(serviceConfigData)
	if err != nil {
		panic("blueprints: invalid service.json: " + err.Error())
	}
}
