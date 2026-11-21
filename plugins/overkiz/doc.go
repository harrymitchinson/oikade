// Package overkiz is the plugin for Overkiz, the platform behind Somfy TaHoma and Atlantic's
// Cozytouch, and for Magellan, Atlantic's own cloud beside it. A home imports it once per hub or
// account:
//
//	home.Plugin("tahoma", overkiz.New(overkiz.Local{
//		Address: oikade.Var("TAHOMA_ADDRESS"), PIN: oikade.Var("TAHOMA_PIN"), Token: oikade.Var("TAHOMA_TOKEN"),
//	}))
//	home.Plugin("cozytouch", overkiz.New(overkiz.Cozytouch{
//		Brand: overkiz.Thermor, Username: oikade.Var("COZYTOUCH_USER"), Password: oikade.Var("COZYTOUCH_PASSWORD"),
//	}))
//
// It speaks Overkiz's enduser API, which a hub's local API and the cloud share (client). Local is a
// hub, over HTTPS with the bearer token its Developer Mode makes: hubTLS checks the hub's
// certificate against Overkiz's root CA, embedded, and its gateway-<pin>.local name in the CN or
// the SANs. Cozytouch logs in with three calls: Atlantic's password grant, a JWT for its token, and
// an Overkiz session that a cookie keeps.
//
// A session reads /setup/devices, registers an event listener and fetches it every poll interval,
// or every busy one while an execution runs. It reads the setup again only when a device is added,
// removed or updated, an execution fails, or the server has forgotten the listener. A device's ID
// is <protocol>.<address> from its device URL, leaving out the hub, and its sub-devices (#2 and on)
// fold into it. models maps a widget to a model, which reads its capabilities from the states of
// the device and its sub-devices and picks its commands from them. A widget it doesn't have isn't
// reported, and is logged once. Set returns once the server has taken the execution, before the
// device acts: its events then show it.
//
// follower keeps the sessions going, and classify sorts their failures by errorCode and message.
// refused waits timing's refused, an hour, as too many failed logins could lock the account and a
// new credential comes with a deploy, which restarts the plugin. limited waits timing's limited. A
// forgotten listener is registered again, and absent, a cloud with no account for the login, isn't
// a problem and is asked again hourly. Anything else backs off from retry to retryMax. Each problem
// is the plugin's status error. While there's no session, every device is offline, the discovered
// list is nil and a write fails at once. Only a Cozytouch session that lasted at least retryMax
// and then ended with a refusal logs in again at once, with its devices online. One that ends
// sooner backs off, doubling, as a cloud that keeps ending them shouldn't be asked over and over.
//
// A Cozytouch account's devices are on Overkiz, on Magellan or on both, so clouds follows a bridge
// and a magellan side by side, each with its own follower. One signer holds Atlantic's token and
// any refusal for both, so they log in once between them, and a share merges what each reports.
// Magellan has no events: setupviewv2 is read every read interval, and after a write once its
// executions are done. Its devices' IDs are magellan.<deviceId>, and magellanModels maps one by its
// modelId. It refuses a write to a device while the last one runs, so writes wait in a queue for
// each device.
//
// The README beside this file is the plugin's user docs.
package overkiz
