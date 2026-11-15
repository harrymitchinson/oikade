// Package overkiz is the plugin for Overkiz, the platform behind Somfy TaHoma and others. A home
// imports it once per hub or account:
//
//	framework.NewHome(framework.Options{},
//		framework.NewPlugin("tahoma", overkiz.New(overkiz.Local{
//			Address: "192.168.1.40:8443", PIN: "1234-5678-9012", TokenSecret: "tahoma-token",
//		})),
//		framework.NewPlugin("cozytouch", overkiz.New(overkiz.Cozytouch{
//			Brand: overkiz.Thermor, Username: "someone@example.com", PasswordSecret: "cozytouch",
//		})),
//	)
//
// Local is a TaHoma hub's local API, with the token its Developer Mode makes, and Cozytouch an
// account in Atlantic's, Thermor's or Sauter's app, over the cloud, with its password. Each is kept
// in a secret and read at each login. A hub's certificate is checked against Overkiz's root CA, and
// its name against the hub's PIN. Every device the hub lists is reported by an ID from its device URL,
// <protocol>.<address>, which leaves out the hub, so a replaced hub keeps its devices. A device's
// sub-devices, such as a sensor inside it, are folded into it.
//
// What a kind of device can do is one entry in models, by its Overkiz widget. A widget the table
// doesn't have isn't reported, and is logged once by name.
//
// While the hub or cloud doesn't answer, every device is offline and the plugin's status says why, and it
// tries again with backoff. A refused credential is tried again once the secret changes, or hourly.
//
// The README beside this file says how to set it up.
package overkiz
