package v1_test

import (
	"fmt"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// A plugin's topics are all under its root.
func ExampleRoot() {
	root := contractv1.NewRoot("simswitch")
	fmt.Println(root.Status())
	fmt.Println(root.Devices())
	fmt.Println(root.Availability("relay-1"))
	fmt.Println(root.State("relay-1"))
	fmt.Println(root.Set("relay-1"))
	// Output:
	// oikade/v1/simswitch/status
	// oikade/v1/simswitch/devices
	// oikade/v1/simswitch/relay-1/availability
	// oikade/v1/simswitch/relay-1/state
	// oikade/v1/simswitch/relay-1/set
}

// A set command is one capability of one device.
func ExampleRoot_ParseSet() {
	root := contractv1.NewRoot("simswitch")
	cmd, ok, err := root.ParseSet(root.Set("relay-1"), []byte(`{"on": true}`))
	fmt.Println(cmd.ID, cmd.Capability, cmd.Value, ok, err)

	_, _, err = root.ParseSet(root.Set("relay-1"), []byte(`{"on": true, "brightness": 40}`))
	fmt.Println(err != nil)
	// Output:
	// relay-1 on true true <nil>
	// true
}
