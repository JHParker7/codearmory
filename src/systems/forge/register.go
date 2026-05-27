package main

import (
	_ "embed"

	"github.com/code-armory-app/codearmory_sdk/registry"
)

//go:embed service.json
var serviceConfigData []byte

var serviceConfig registry.ServiceConfig

func init() {
	var err error
	serviceConfig, err = registry.LoadConfig(serviceConfigData)
	if err != nil {
		panic("forge: invalid service.json: " + err.Error())
	}
}
