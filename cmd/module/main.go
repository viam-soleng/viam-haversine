// Command module is the entrypoint for the viam-soleng:haversine:haversine module.
package main

import (
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"viam-haversine/models"
)

func main() {
	module.ModularMain(resource.APIModel{API: sensor.API, Model: models.Model})
}
