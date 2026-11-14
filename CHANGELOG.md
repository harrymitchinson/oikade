# Changelog

## [0.2.0-alpha.2](https://github.com/harrymitchinson/oikade/compare/v0.2.0-alpha.1...v0.2.0-alpha.2) (2026-11-14)


### ⚠ BREAKING CHANGES

* **framework:** the exported fields of Hold, Moment, Flow, Control, Plugin, Window and Budget are gone, as is the Prefer type. Rewrite each rule with NewHold, NewMoment, NewFlow, NewControl, NewPlugin, NewWindow, NewBudget or NewBandedBudget.
* **framework:** Options.Latitude and Options.Longitude are replaced by Options.Location. Write framework.Options{Location: framework.NewLocation(51.48, -0.01)}.
* **framework:** Home's fields are private and the Site type is gone. Replace framework.Home{Holds: ..., Site: &framework.Site{...}} with framework.NewHome(framework.Options{Latitude: ..., Longitude: ...}, parts...).
* **contract:** contractv1.NewRoot takes only the plugin's name, and OIKADE_TOPIC and every topic lose their namespace segment. An outside plugin given a root by hand needs oikade/v1/<name>.
* **engine:** the namespace label is gone from every oikade_ metric, and the dashboard's $namespace variable with it. Saved state and recorded driver names lose their "home/" segment, so the store's format is now 2 and a host refuses a format 1 data directory with ErrFormat.

### Features

* **cli:** say what oikade set and pause did, not what they asked ([#238](https://github.com/harrymitchinson/oikade/issues/238)) ([52fcab3](https://github.com/harrymitchinson/oikade/commit/52fcab33c68378ea9f55917e3a7a7352126d6b9c))
* **cli:** say what would fill an empty answer ([#241](https://github.com/harrymitchinson/oikade/issues/241)) ([a81edbd](https://github.com/harrymitchinson/oikade/commit/a81edbd0e0c457649218f7f686ee88e142420bef))
* **cli:** say when oikade tail has started following ([#239](https://github.com/harrymitchinson/oikade/issues/239)) ([d5b7998](https://github.com/harrymitchinson/oikade/commit/d5b7998a965c36129922eead0e3086a5ed8851fa))
* **cli:** show progress during oikade deploy ([#234](https://github.com/harrymitchinson/oikade/issues/234)) ([8b8c0ea](https://github.com/harrymitchinson/oikade/commit/8b8c0ea000c98a19136a7a44cd0580d4734cf890))
* **cli:** write the CLI output rule, and keep Go's lines out of oikade init ([#235](https://github.com/harrymitchinson/oikade/issues/235)) ([d776b22](https://github.com/harrymitchinson/oikade/commit/d776b225dacfe07729e4f05b928a8cbd94cd3947))
* **contract:** drop the namespace from plugin topic roots ([#251](https://github.com/harrymitchinson/oikade/issues/251)) ([b0fc2ba](https://github.com/harrymitchinson/oikade/commit/b0fc2ba33ee558bf9de7268bf1cca55b22f04038))
* **framework:** build rules with constructors ([#254](https://github.com/harrymitchinson/oikade/issues/254)) ([fc63c61](https://github.com/harrymitchinson/oikade/commit/fc63c6119362ce6b27187d03200f7676a378722f))
* **framework:** compose a home from parts ([#252](https://github.com/harrymitchinson/oikade/issues/252)) ([24af281](https://github.com/harrymitchinson/oikade/commit/24af2812c94f8cdb0f7b004538651642061937d1))
* **framework:** require both coordinates of a home's location ([#253](https://github.com/harrymitchinson/oikade/issues/253)) ([ca0b2ab](https://github.com/harrymitchinson/oikade/commit/ca0b2ab8c73ab8963100a70a95b235f1db4fa29d))
* **host:** keep a flow's run and claims when a deploy fails ([#226](https://github.com/harrymitchinson/oikade/issues/226)) ([3337b57](https://github.com/harrymitchinson/oikade/commit/3337b579fbb00ed6aaef8ff65f6bbb6900ef8fe3))
* **host:** say which devices a dropped plugin leaves behind, and remove them in one go ([#231](https://github.com/harrymitchinson/oikade/issues/231)) ([5024a57](https://github.com/harrymitchinson/oikade/commit/5024a570de2be9096074f221a73929955c1afa01))
* **host:** show a write that Safety refused ([#236](https://github.com/harrymitchinson/oikade/issues/236)) ([ef90e04](https://github.com/harrymitchinson/oikade/commit/ef90e04906848e621d315cb2559354a65d111224))
* **host:** show whether a hold is active, and why, in oikade status ([#233](https://github.com/harrymitchinson/oikade/issues/233)) ([fe8d45f](https://github.com/harrymitchinson/oikade/commit/fe8d45fc3993a8bca33f0037700c37f808f48f8c))
* **host:** stop in order, within a deadline ([#245](https://github.com/harrymitchinson/oikade/issues/245)) ([936b766](https://github.com/harrymitchinson/oikade/commit/936b766f32bf3bb129fe673a886ee0d658f7c2dd))
* **matter:** end sessions before the bridge stops ([#246](https://github.com/harrymitchinson/oikade/issues/246)) ([6c7fb62](https://github.com/harrymitchinson/oikade/commit/6c7fb622bcd09d29193f9a2222d98343786c29a6))
* **matter:** find a node by operational discovery over mDNS ([#256](https://github.com/harrymitchinson/oikade/issues/256)) ([846cedf](https://github.com/harrymitchinson/oikade/commit/846cedf932020acb19b4a2852c7785b9fd8d2e39))
* **matter:** grant a subscription's max interval of at most 60 s ([#255](https://github.com/harrymitchinson/oikade/issues/255)) ([b1e0ede](https://github.com/harrymitchinson/oikade/commit/b1e0ede663f54dd7072c62c04cf01a48c9650f00))
* **matter:** log subscriptions at Info with their min and max intervals ([#230](https://github.com/harrymitchinson/oikade/issues/230)) ([f412b62](https://github.com/harrymitchinson/oikade/commit/f412b6216756d2fd130320bb103116334d72d220))
* **matter:** save subscriptions and resume them after a restart ([#257](https://github.com/harrymitchinson/oikade/issues/257)) ([885704b](https://github.com/harrymitchinson/oikade/commit/885704b9ae02e2b9f7ce9e3d6d91eadb8b0735e6))
* **plan:** merge a PR behind main when main hasn't touched its files ([#240](https://github.com/harrymitchinson/oikade/issues/240)) ([bb2e317](https://github.com/harrymitchinson/oikade/commit/bb2e3174282b081b8fb398793326ffe7b3f7431d))
* **plan:** steps as a checklist in the feature, not sub-issues ([#264](https://github.com/harrymitchinson/oikade/issues/264)) ([f25fb3c](https://github.com/harrymitchinson/oikade/commit/f25fb3c86af1295e1307c85c2e8ff1b476cca854))
* **plugins:** add the eWeLink LAN plugin ([#261](https://github.com/harrymitchinson/oikade/issues/261)) ([edd37b3](https://github.com/harrymitchinson/oikade/commit/edd37b3f5c439480575ee5e119314248f2880bfd))


### Bug Fixes

* **cli:** deploy's counter on dumb terminals, its upload race, and replay ending in EOF ([#243](https://github.com/harrymitchinson/oikade/issues/243)) ([143b603](https://github.com/harrymitchinson/oikade/commit/143b60379608b2752830d8daee3b376ef2087d18))
* **engine:** a hold's past delay no longer stops the engine taking changes ([#221](https://github.com/harrymitchinson/oikade/issues/221)) ([c7853c5](https://github.com/harrymitchinson/oikade/commit/c7853c5f08d99bb3ec65430b08ae92ac0bb16e95))
* **engine:** an active room hold takes up members that join it ([#225](https://github.com/harrymitchinson/oikade/issues/225)) ([c338475](https://github.com/harrymitchinson/oikade/commit/c33847570d6891ac35c62c69c326f38a06a9a90c))
* **engine:** take in a report as a whole before any hold writes ([#219](https://github.com/harrymitchinson/oikade/issues/219)) ([f1955a3](https://github.com/harrymitchinson/oikade/commit/f1955a3fa23ba4d0e064b98733a6d3672d21832d))
* **engine:** warn when a hold reads a room with no members ([#222](https://github.com/harrymitchinson/oikade/issues/222)) ([462aba8](https://github.com/harrymitchinson/oikade/commit/462aba8b7d6bbeab704dcff63c0ca30766d0304c))
* **host:** count the devices on record in Started host ([#229](https://github.com/harrymitchinson/oikade/issues/229)) ([d72adbf](https://github.com/harrymitchinson/oikade/commit/d72adbf9c8783e9a5b84c1d3f048501a988ba8c4))
* **host:** end an idle tail with its stream, not a TLS error ([#237](https://github.com/harrymitchinson/oikade/issues/237)) ([20cdbd7](https://github.com/harrymitchinson/oikade/commit/20cdbd7e98a34eb11dc1538a74b7903ed693d4d4))
* **host:** keep a refused deploy off the record and close flowcheck gaps ([#223](https://github.com/harrymitchinson/oikade/issues/223)) ([4535072](https://github.com/harrymitchinson/oikade/commit/45350722f7daf68de73bc06edc7e572670f3d896))
* **host:** take a rounded report as oikade set's answer ([#260](https://github.com/harrymitchinson/oikade/issues/260)) ([78c0b9e](https://github.com/harrymitchinson/oikade/commit/78c0b9ed4ff17db2cb17cbe460ac30806aad481f))
* **host:** take set's answer past a fade, and status's deploying from one lock ([#244](https://github.com/harrymitchinson/oikade/issues/244)) ([0697787](https://github.com/harrymitchinson/oikade/commit/069778779c47cebd2f6162bbc4bf7efff7889622))
* **logging:** drop test log lines once the test has ended ([#232](https://github.com/harrymitchinson/oikade/issues/232)) ([8dbcf89](https://github.com/harrymitchinson/oikade/commit/8dbcf89af70a3297c640658a14d1fcdc45fa89e5))


### Performance Improvements

* **engine:** cache sunrise and sunset per day ([#248](https://github.com/harrymitchinson/oikade/issues/248)) ([310ec5d](https://github.com/harrymitchinson/oikade/commit/310ec5d8578bcabd736d0f5bed5f7b2fe59d6bae))


### Code Refactoring

* **engine:** drop the namespace from ident.ID ([#224](https://github.com/harrymitchinson/oikade/issues/224)) ([64c7252](https://github.com/harrymitchinson/oikade/commit/64c7252cef264cffadc71dcb2c980a683d7bcf1b))

## [0.2.0-alpha.1](https://github.com/harrymitchinson/oikade/compare/v0.1.0...v0.2.0-alpha.1) (2026-11-07)


### ⚠ BREAKING CHANGES

* **framework:** Moment.At, Flow.At, Window.Start and Window.End take framework.Time instead of string: write framework.Clock("23:30").
* **framework:** the host protocol is version 6, so a binary built against version 5 is refused until it's rebuilt.
* every capability name changes, with no aliases: homes, plugins, PromQL and the bundled dashboard. sdk/plugin's RelayCapability is gone, because a relay reports on whatever it switches.
* the CLI's binary, its cmd/ path and its release archives are oikade, not oikadectl.
* **release:** release assets are oikadectl-<os>-<arch>.tar.gz and oikade-host-linux-<arch>.tar.gz, not bare binaries.
* **framework:** Hold, Set, Change, Control and Bands lose their Device, Room, Capability and Value fields, State.Bool and State.Number become State.Get, hometest takes handles, and devices.go has a value per device rather than a string constant.
* the oikade binary is gone. Run oikadectl for every command, and oikade-host in place of oikade host: a systemd unit's ExecStart, and the image, whose entrypoint is now oikade-host with no host argument. client add is oikade-host client add, on the host's machine.
* --host, oikade proxy, oikade install and oikade upgrade are gone, as is make test-install. Reach a host with oikade login, and run it under your own systemd unit.
* the Kubernetes install, every CRD, the manager binary and install.yaml are gone. A home is a Go module run by oikade host.

### Features

* **adopt:** adopt discovered devices as Devices ([#168](https://github.com/harrymitchinson/oikade/issues/168)) ([63ca84e](https://github.com/harrymitchinson/oikade/commit/63ca84e47d362b999dce6b138994a24c86249796))
* **adopt:** copy a device's name in its app once, and never follow it ([#173](https://github.com/harrymitchinson/oikade/issues/173)) ([8ed2170](https://github.com/harrymitchinson/oikade/commit/8ed21701b3ebf3a5a31f63e9402ccd1ea67ead94))
* **adopt:** removal policy for adopted Devices ([#169](https://github.com/harrymitchinson/oikade/issues/169)) ([0cce30b](https://github.com/harrymitchinson/oikade/commit/0cce30b7b0b19b1012eff0d96290837dd1b2a007))
* **adopt:** replace a reset device's ID with oikade.dev/replace-with ([#170](https://github.com/harrymitchinson/oikade/issues/170)) ([1731731](https://github.com/harrymitchinson/oikade/commit/1731731978bb5db3a859c5885e502bf21e0eadd1))
* **api:** serve the host API over HTTP on its socket ([#193](https://github.com/harrymitchinson/oikade/issues/193)) ([0267363](https://github.com/harrymitchinson/oikade/commit/0267363bfc53e1735eb20d50010b05501ed416ac))
* **api:** serve the host API over TLS, with a token per client ([#194](https://github.com/harrymitchinson/oikade/issues/194)) ([52d3cfa](https://github.com/harrymitchinson/oikade/commit/52d3cfab9a38d6b42ed2e835603266cd5abd82f7))
* **api:** spec.discovery on Plugin and Zigbee2MQTT ([#166](https://github.com/harrymitchinson/oikade/issues/166)) ([bdce604](https://github.com/harrymitchinson/oikade/commit/bdce604685da68c427a264b733f6a924ee2396fd))
* **cli:** oikade init, check, dev and test ([#186](https://github.com/harrymitchinson/oikade/issues/186)) ([4474395](https://github.com/harrymitchinson/oikade/commit/4474395c5201c6ea270770665c6faf4de2757249))
* **cli:** oikade login, with a pinned host and contexts ([#195](https://github.com/harrymitchinson/oikade/issues/195)) ([9652506](https://github.com/harrymitchinson/oikade/commit/96525063a41ea1cdd316b08c8ea130204c21497b))
* **contract:** device types from Zigbee2MQTT and the plugin contract ([#209](https://github.com/harrymitchinson/oikade/issues/209)) ([1d89792](https://github.com/harrymitchinson/oikade/commit/1d89792382f3e2c4441857f925b55188408fafec))
* **contract:** discovered devices with stable IDs ([#165](https://github.com/harrymitchinson/oikade/issues/165)) ([f8af2a3](https://github.com/harrymitchinson/oikade/commit/f8af2a3acf6ea7d6b9841a076d1e472cb0dfd42a))
* **deploy:** stream the binary to the host with the call ([#196](https://github.com/harrymitchinson/oikade/issues/196)) ([c7edfd0](https://github.com/harrymitchinson/oikade/commit/c7edfd015b82fb651abc83a0704c2ed5848b8075))
* **devices:** device types in the registry, oikade devices type and the Matter mapping ([#208](https://github.com/harrymitchinson/oikade/issues/208)) ([02e12f6](https://github.com/harrymitchinson/oikade/commit/02e12f6122f458c2ad72e53ceaec03347e8543e8))
* **engine:** know exactly when the engine is idle ([#217](https://github.com/harrymitchinson/oikade/issues/217)) ([7c6633c](https://github.com/harrymitchinson/oikade/commit/7c6633c9e36acb6f71f26e6cee2aa60896cc27b7))
* **engine:** run rules written in Go ([#177](https://github.com/harrymitchinson/oikade/issues/177)) ([b68febf](https://github.com/harrymitchinson/oikade/commit/b68febf22c8f70b8be6cd75d2ae01ba50167b60d))
* **flow:** run durable flows in Go ([#179](https://github.com/harrymitchinson/oikade/issues/179)) ([24b75ad](https://github.com/harrymitchinson/oikade/commit/24b75add93c5c22622fc5353e11db19fea929179))
* **framework:** Home.Site, Sunrise and Sunset ([#213](https://github.com/harrymitchinson/oikade/issues/213)) ([c19bf75](https://github.com/harrymitchinson/oikade/commit/c19bf752ee8368566c08fa8176b0fad4bf5ba788))
* **framework:** limit room rules to device types ([#210](https://github.com/harrymitchinson/oikade/issues/210)) ([a3e96ec](https://github.com/harrymitchinson/oikade/commit/a3e96ec610233b4336e8f7c9625ad52cc29bb83e))
* **framework:** State.Lookup and room reads in conditions ([#212](https://github.com/harrymitchinson/oikade/issues/212)) ([5b57e24](https://github.com/harrymitchinson/oikade/commit/5b57e24a8255c6a614a7ae4145835b9392adc5da))
* **framework:** State.Within, a hold's window in a condition ([#214](https://github.com/harrymitchinson/oikade/issues/214)) ([e7e752e](https://github.com/harrymitchinson/oikade/commit/e7e752ee8df5be652d87d0a24321dcc9de23ca3e))
* **framework:** typed capability handles, from oikadectl types ([#202](https://github.com/harrymitchinson/oikade/issues/202)) ([e37f499](https://github.com/harrymitchinson/oikade/commit/e37f499fb52901e75a4b93da4027f5dc20ae9854))
* **framework:** typed clock times in At and Window ([#211](https://github.com/harrymitchinson/oikade/issues/211)) ([e373306](https://github.com/harrymitchinson/oikade/commit/e3733069d28e57d153725866efd23068e9d88010))
* **host:** deploy and roll back your code ([#180](https://github.com/harrymitchinson/oikade/issues/180)) ([1d14e93](https://github.com/harrymitchinson/oikade/commit/1d14e938d6365fd672719cdb598f94704fc20535))
* **host:** keep a device registry, with oikade devices and oikade types ([#182](https://github.com/harrymitchinson/oikade/issues/182)) ([3fc4d9f](https://github.com/harrymitchinson/oikade/commit/3fc4d9fc2f000efc73d8b77d79d9d60d19ff18bc))
* **host:** run automations in a process apart from the host ([#178](https://github.com/harrymitchinson/oikade/issues/178)) ([7981856](https://github.com/harrymitchinson/oikade/commit/7981856c7596a467685ab901e37aabc223fc575a))
* **host:** run imported plugins, one process each ([#184](https://github.com/harrymitchinson/oikade/issues/184)) ([523a2bd](https://github.com/harrymitchinson/oikade/commit/523a2bd0158b307abf08e79c54094c30eb1f5e98))
* **host:** run the Matter bridge in the host ([#183](https://github.com/harrymitchinson/oikade/issues/183)) ([c140fbf](https://github.com/harrymitchinson/oikade/commit/c140fbfecb02675c91670de470118732164afc5d))
* **host:** status, set, tail and pause, over SSH too ([#181](https://github.com/harrymitchinson/oikade/issues/181)) ([94344cb](https://github.com/harrymitchinson/oikade/commit/94344cbd3f9f948c2bddd4575a98d94a1222d740))
* **install:** oikade install, upgrade, backup and secrets ([#188](https://github.com/harrymitchinson/oikade/issues/188)) ([9272475](https://github.com/harrymitchinson/oikade/commit/9272475e80e72d8acc4c2bc6bf292b490797915f))
* **matter:** contain a panic in handling one message ([#199](https://github.com/harrymitchinson/oikade/issues/199)) ([bf8fa4f](https://github.com/harrymitchinson/oikade/commit/bf8fa4f86f42ad24d5477b38b15c979142b605a3))
* **matter:** export the bridge's metrics, with a Grafana row ([#198](https://github.com/harrymitchinson/oikade/issues/198)) ([e952bc5](https://github.com/harrymitchinson/oikade/commit/e952bc5f0550b6fb02dbcaf74c9eae22721b0a73))
* **matter:** rename a bridged device in place on a displayName change ([#172](https://github.com/harrymitchinson/oikade/issues/172)) ([964f993](https://github.com/harrymitchinson/oikade/commit/964f993dfe05071fe8ffded132191f4980dbee64))
* name capabilities for people ([#205](https://github.com/harrymitchinson/oikade/issues/205)) ([d615391](https://github.com/harrymitchinson/oikade/commit/d615391fcbb83ac4f5fae1681974f56c6ff6dbd4))
* **release:** build releases and the image with GoReleaser ([#203](https://github.com/harrymitchinson/oikade/issues/203)) ([7422c4b](https://github.com/harrymitchinson/oikade/commit/7422c4be0210b08bce53a9b0925a9390985d838a))
* remove SSH, oikade install and oikade upgrade ([#197](https://github.com/harrymitchinson/oikade/issues/197)) ([1b95167](https://github.com/harrymitchinson/oikade/commit/1b951676b7ae0182200791c41f04fde44c5c931b))
* remove the operator and CRDs ([#189](https://github.com/harrymitchinson/oikade/issues/189)) ([aa98a30](https://github.com/harrymitchinson/oikade/commit/aa98a30f4735ca16cfb64baeb05405e7d43992f0))
* rename oikadectl to oikade ([#204](https://github.com/harrymitchinson/oikade/issues/204)) ([41b9343](https://github.com/harrymitchinson/oikade/commit/41b9343e1257ad095aed396657d5d49fd54f8ff3))
* **replay:** oikade replay and a bounded event log ([#187](https://github.com/harrymitchinson/oikade/issues/187)) ([afd9fb1](https://github.com/harrymitchinson/oikade/commit/afd9fb17ce1eff779a46be6a2b030afe8ae57bdc))
* **runtime:** discovered devices in Plugin and Zigbee2MQTT status ([#167](https://github.com/harrymitchinson/oikade/issues/167)) ([d476c49](https://github.com/harrymitchinson/oikade/commit/d476c49734753027e9b79dfff9394c4a83c3836d))
* split oikade into oikadectl and oikade-host ([#201](https://github.com/harrymitchinson/oikade/issues/201)) ([95f0f30](https://github.com/harrymitchinson/oikade/commit/95f0f30096cb6d6fc09b98ec8c6dd11e44b2ba76))
* **store:** add the embedded store and write-ahead journal ([#176](https://github.com/harrymitchinson/oikade/issues/176)) ([67d9a46](https://github.com/harrymitchinson/oikade/commit/67d9a46720b5d625c97bd1b9fdd31f9f37dc2bd4))
* **zigbee2mqtt:** ask for each value nothing retained, after a device list ([#200](https://github.com/harrymitchinson/oikade/issues/200)) ([2c415e8](https://github.com/harrymitchinson/oikade/commit/2c415e8f81eb7d5a3a0c4e04222363093b576fbf))
* **zigbee2mqtt:** run Zigbee as a plugin on a Zigbee2MQTT you run ([#185](https://github.com/harrymitchinson/oikade/issues/185)) ([21fa5c5](https://github.com/harrymitchinson/oikade/commit/21fa5c5dcec7fbc552a7ba419ae7da25405abf7d))
* **zigbee2mqtt:** stop writing friendly names into Zigbee2MQTT ([#171](https://github.com/harrymitchinson/oikade/issues/171)) ([887c60b](https://github.com/harrymitchinson/oikade/commit/887c60bac9803f5a03e71ad32001eb2513e0bf23))


### Bug Fixes

* **matterbridge:** send a device's bursts in order ([#216](https://github.com/harrymitchinson/oikade/issues/216)) ([5fa5923](https://github.com/harrymitchinson/oikade/commit/5fa5923088a688a475c81fc1510d1caf6b1fc3b9))


### Documentation

* **deploy:** install v0.1.0, and say later versions are v0.2.0 alphas ([#163](https://github.com/harrymitchinson/oikade/issues/163)) ([530f059](https://github.com/harrymitchinson/oikade/commit/530f05905a09bf64990bc556e1fc178e1ffdcea2))

## [0.1.0](https://github.com/harrymitchinson/oikade/compare/v0.1.0-alpha.1...v0.1.0) (2026-11-04)


### ⚠ BREAKING CHANGES

* remove HomeKit ([#150](https://github.com/harrymitchinson/oikade/issues/150))

### Features

* **conformance:** run the plugin contract suite as oikade-conformance ([#154](https://github.com/harrymitchinson/oikade/issues/154)) ([72b105b](https://github.com/harrymitchinson/oikade/commit/72b105bc7a2d6efc10f76bfc5c28dad05bbaa145))
* **hack:** retitle issues and rename projects with hack/plan ([#153](https://github.com/harrymitchinson/oikade/issues/153)) ([51c940c](https://github.com/harrymitchinson/oikade/commit/51c940c0d91bba54454ecfdb8461087cf572064c))
* **matter:** add an mDNS responder for commissionable and operational services ([#138](https://github.com/harrymitchinson/oikade/issues/138)) ([5cb9e2e](https://github.com/harrymitchinson/oikade/commit/5cb9e2eb27df9bc90ae35de31f7bc585c885e987))
* **matter:** add operational certificates ([#139](https://github.com/harrymitchinson/oikade/issues/139)) ([4a6dca4](https://github.com/harrymitchinson/oikade/commit/4a6dca4f1bcd7d0eafdd0af25e2cba35f73c3d2c))
* **matter:** add the matter.oikade.dev Bridge API and Device.spec.matter ([#144](https://github.com/harrymitchinson/oikade/issues/144)) ([f372cbf](https://github.com/harrymitchinson/oikade/commit/f372cbfb8a93ffc1ccef4fc1d9beecd7427b4674))
* **matter:** CASE sessions with resumption ([#140](https://github.com/harrymitchinson/oikade/issues/140)) ([2ff9884](https://github.com/harrymitchinson/oikade/commit/2ff9884e71f93beceb6c08df3837f0ec60f5f437))
* **matter:** commissioning clusters, device attestation and access control ([#141](https://github.com/harrymitchinson/oikade/issues/141)) ([70c4cb1](https://github.com/harrymitchinson/oikade/commit/70c4cb1662ac2f4f685188b95e65d292518a8c38))
* **matter:** encode and decode Matter TLV ([#132](https://github.com/harrymitchinson/oikade/issues/132)) ([b5583fe](https://github.com/harrymitchinson/oikade/commit/b5583fe9c4008ba86691055a3454f1d90537ef19))
* **matter:** keep the pairing codes in the Bridge's Secret, not its status ([#148](https://github.com/harrymitchinson/oikade/issues/148)) ([e802e73](https://github.com/harrymitchinson/oikade/commit/e802e73e7ac15cea9c488baeffef31ac0e78f9ca))
* **matter:** manager matter-spike, commissioned by chip-tool ([#142](https://github.com/harrymitchinson/oikade/issues/142)) ([31df930](https://github.com/harrymitchinson/oikade/commit/31df930bcafce138fab955a35cd1b2f7e2425c57))
* **matter:** map Devices to Matter device types, and take writes from controllers ([#147](https://github.com/harrymitchinson/oikade/issues/147)) ([2a024c6](https://github.com/harrymitchinson/oikade/commit/2a024c6998ae27a2188ebd9f59eaf9b36a435ec7))
* **matter:** Matter Bridge controller and the manager matter role ([#145](https://github.com/harrymitchinson/oikade/issues/145)) ([75b4e5f](https://github.com/harrymitchinson/oikade/commit/75b4e5fd39a6fae5a817a2f42c6a86db0cb0bd15))
* **matter:** PASE, AES-CCM sessions and the setup payload ([#135](https://github.com/harrymitchinson/oikade/issues/135)) ([e39e9ca](https://github.com/harrymitchinson/oikade/commit/e39e9ca22e5610404f98f78a840e7030b0558451))
* **matter:** the Interaction Model, and a bridge with one on/off endpoint ([#136](https://github.com/harrymitchinson/oikade/issues/136)) ([f5f5f52](https://github.com/harrymitchinson/oikade/commit/f5f5f5208957e7a1a06f37351193674146a30a56))
* **matter:** the message layer and MRP over UDP ([#134](https://github.com/harrymitchinson/oikade/issues/134)) ([9576edb](https://github.com/harrymitchinson/oikade/commit/9576edb4bf3d258158fcfe3b3a80053fa561ae93))
* **plan:** build steps in plugin repos ([#158](https://github.com/harrymitchinson/oikade/issues/158)) ([29e55ad](https://github.com/harrymitchinson/oikade/commit/29e55ad53d824261a672ac48a6f32dc767c75884))
* **plan:** drop the review floor and narrow the gh denials ([#157](https://github.com/harrymitchinson/oikade/issues/157)) ([bd5438c](https://github.com/harrymitchinson/oikade/commit/bd5438cc9e34ba95898221599482187213bbc749))
* remove HomeKit ([#150](https://github.com/harrymitchinson/oikade/issues/150)) ([2b71429](https://github.com/harrymitchinson/oikade/commit/2b7142900ffd2836bb8557c5f11f4198c8e09bd1))


### Bug Fixes

* **matter:** accept an empty ICAC in AddNOC, as Apple Home sends ([#143](https://github.com/harrymitchinson/oikade/issues/143)) ([fa6903a](https://github.com/harrymitchinson/oikade/commit/fa6903a1beb0ec4d39afbc5bf72a6f8d7bb49875))
* **plan:** merge approved steps after the feature's claim is released ([#152](https://github.com/harrymitchinson/oikade/issues/152)) ([26a4568](https://github.com/harrymitchinson/oikade/commit/26a4568c4a09ef736f7dd7b8937b798c59a0eacb))
* **plan:** merge plugin repos named after this one, and reach them like origin ([#159](https://github.com/harrymitchinson/oikade/issues/159)) ([605ed32](https://github.com/harrymitchinson/oikade/commit/605ed32efc404268df09d3ea776e2666ab8c89a0))


### Documentation

* **versioning:** say what release-please proposes after a milestone ([#162](https://github.com/harrymitchinson/oikade/issues/162)) ([bb2beb7](https://github.com/harrymitchinson/oikade/commit/bb2beb73482d76bb2e1886d3bdad1a855d4dfadb))

## [0.1.0-alpha.1](https://github.com/harrymitchinson/oikade/compare/v0.1.0-alpha.0...v0.1.0-alpha.1) (2026-10-26)


### ⚠ BREAKING CHANGES

* **rule:** moments are triggers, not on
* **rule:** rename Automation to Rule (0060)

### Features

* add capability registry, MQTT client and zigbee2mqtt driver ([84b94dd](https://github.com/harrymitchinson/oikade/commit/84b94dd2955610663438a1efceb2c8047417416d))
* **api:** add the Plugin kind and Device.spec.plugin ([#8](https://github.com/harrymitchinson/oikade/issues/8)) ([82910df](https://github.com/harrymitchinson/oikade/commit/82910df011ee8cace046489491444e8010d37754))
* **api:** define Device, Room, Automation, Bridge and Zigbee2MQTT ([3751951](https://github.com/harrymitchinson/oikade/commit/37519519e44468decd33a9775983268f792d46c9))
* **ci:** approved counts only when the owner adds it ([8b8261b](https://github.com/harrymitchinson/oikade/commit/8b8261b58e2633fa45a5a1db7a38ca0ec6184a37))
* **ci:** needs-review PRs wait for the owner's Approve ([985292b](https://github.com/harrymitchinson/oikade/commit/985292bc42481a0bb8e1473bae60dd4ae5733c94)), closes [#108](https://github.com/harrymitchinson/oikade/issues/108)
* **claude:** commit as oikade-agent[bot] and deny raw gh writes ([77f39eb](https://github.com/harrymitchinson/oikade/commit/77f39eb1b11c7369d72989330ef320c00f6216da))
* **contract:** shared oikade/v1 contract types in contract/v1 ([#56](https://github.com/harrymitchinson/oikade/issues/56)) ([2086fc5](https://github.com/harrymitchinson/oikade/commit/2086fc57314eba394f62433c0a093d33777260e8))
* **control:** claim arbiter for arbitrated capabilities (0060) ([e449023](https://github.com/harrymitchinson/oikade/commit/e449023a7f0d87400ded7fd152b37c4c343fe6e7))
* **device:** Device.spec.control API and validation (0060) ([659819b](https://github.com/harrymitchinson/oikade/commit/659819b1c56ba7969104a84f6e24c10fb563368a))
* **driver:** add the oikade/v1 plugin contract and its decoder ([#5](https://github.com/harrymitchinson/oikade/issues/5)) ([2348f9c](https://github.com/harrymitchinson/oikade/commit/2348f9c778e959fd734db912f36c7bf84574578d))
* **hack:** hack/plan writes as oikade-agent[bot] inside Claude Code ([860c8de](https://github.com/harrymitchinson/oikade/commit/860c8de8f534b029564c7bc7d1a2ff5958fd38f0))
* **hack:** merge needs-review PRs once the owner approves them ([#116](https://github.com/harrymitchinson/oikade/issues/116)) ([1223d9f](https://github.com/harrymitchinson/oikade/commit/1223d9fcbf2e4d06e1167cec17ac33460bae37da))
* **hack:** plan commands for every write skills make ([1aa9840](https://github.com/harrymitchinson/oikade/commit/1aa98403a1412ee7e562ce8704fcd3b2e5817607))
* **hack:** plan in Linear instead of GitHub issues ([#117](https://github.com/harrymitchinson/oikade/issues/117)) ([4bc4b71](https://github.com/harrymitchinson/oikade/commit/4bc4b714701d3386ebf069ab31ee42bf6b651ec9))
* **homekit:** expose Devices to HomeKit through a HAP bridge ([d7740ab](https://github.com/harrymitchinson/oikade/commit/d7740ab1885cb4ba4bd96706cad17362f9ea0d12))
* **hub:** follow plugins in the runtime and HAP ([#9](https://github.com/harrymitchinson/oikade/issues/9)) ([5d22e83](https://github.com/harrymitchinson/oikade/commit/5d22e83176eee8502a6cc8295d6a64d23c9864ff))
* **log:** write structured JSON logs with log/slog ([d7b64f3](https://github.com/harrymitchinson/oikade/commit/d7b64f3111ee7f8294c613cef25f55ff9534ed59))
* **mqtt:** Last Will, OnConnect hook and clean Disconnect ([#58](https://github.com/harrymitchinson/oikade/issues/58)) ([a44490f](https://github.com/harrymitchinson/oikade/commit/a44490f74afd95f385f44f5005499bcb37cca2e4))
* package Mosquitto, runtime, Grafana dashboard, samples and e2e ([d0ec556](https://github.com/harrymitchinson/oikade/commit/d0ec556b6ab2b0bfca31453db5dbcfaa2e2dd732))
* **plugins:** Go example-switch on the Go SDK ([a78f0ad](https://github.com/harrymitchinson/oikade/commit/a78f0adc21ca488f1986fe90433151c632cf217f))
* **plugins:** run plugins from the manager ([#11](https://github.com/harrymitchinson/oikade/issues/11)) ([acdf0a9](https://github.com/harrymitchinson/oikade/commit/acdf0a97707ac3f6b85ac8375f17740f2271a79f))
* **release:** version in the binary and the milestone versioning rule (0036) ([59093dd](https://github.com/harrymitchinson/oikade/commit/59093dd7d6ba84b5adf51edb7c4fd9045b8a09ee))
* **rule:** add while.budget, validated against the target's control entry ([#120](https://github.com/harrymitchinson/oikade/issues/120)) ([6a12659](https://github.com/harrymitchinson/oikade/commit/6a12659c4888b558074d0c94e50d0e6394a8870e))
* **rule:** arbitration in the runtime (0060) ([e0ad383](https://github.com/harrymitchinson/oikade/commit/e0ad3837159293715bd1cbe798ea0c9c976e0e39))
* **rule:** holds and claims API and validation (0060) ([03e0dfd](https://github.com/harrymitchinson/oikade/commit/03e0dfd3455bcbb2c594ce91d9717d693bc95e78))
* **rule:** holds in the runtime (0060) ([f6272f3](https://github.com/harrymitchinson/oikade/commit/f6272f3947ce7b4f3f7d5bd2e5438ec862d9f79c))
* **rule:** HomeKit writes as claims (0060) ([3244e5a](https://github.com/harrymitchinson/oikade/commit/3244e5ae7c11043866df85fb288ed1454332a793))
* **rule:** plan budget holds in the runtime ([#122](https://github.com/harrymitchinson/oikade/issues/122)) ([e5abeed](https://github.com/harrymitchinson/oikade/commit/e5abeede00f125076317f34e31087762864ae6bb))
* **rule:** rename Automation to Rule (0060) ([1f91c5c](https://github.com/harrymitchinson/oikade/commit/1f91c5c06371d3ae4085341339982c3b2ff2fe06))
* **rule:** time and sun triggers (0060) ([b3316ca](https://github.com/harrymitchinson/oikade/commit/b3316ca0915ecc6ddf9bacf20c5664cd2059c739))
* **runtime:** add hub, Device status, metrics and automation engine ([6591614](https://github.com/harrymitchinson/oikade/commit/65916141d5f06b74251d42d6b6cfb6b4f58b9c4f))
* **schedule:** planner for budgets, rules and block constraints ([#53](https://github.com/harrymitchinson/oikade/issues/53)) ([1f29610](https://github.com/harrymitchinson/oikade/commit/1f296106d20d4d618296d5155ee87fd97cf1c7fd))
* **sdk:** add the TypeScript plugin SDK and conformance suite ([#12](https://github.com/harrymitchinson/oikade/issues/12)) ([bf81155](https://github.com/harrymitchinson/oikade/commit/bf811550a069a7d52d78fd6297ed9b61e2a95daa))
* **sdk:** Go plugin SDK in sdk/plugin ([8de2b89](https://github.com/harrymitchinson/oikade/commit/8de2b89b75d66cc36bc509a976965e329ac4fbc6))
* **zigbee:** render and run Zigbee2MQTT from declared Devices ([1221f27](https://github.com/harrymitchinson/oikade/commit/1221f277af61adadab5d909c48cb54af65524bc2))


### Bug Fixes

* **ci:** a review reruns the review gate instead of adding a check ([4b53baf](https://github.com/harrymitchinson/oikade/commit/4b53baff502ea0142128a4f0e918c977acd52a63))
* **ci:** let the review gate fetch an approved commit ([ecc0755](https://github.com/harrymitchinson/oikade/commit/ecc0755da0f957ed21a18e8addf8df09d644a1d4))
* **ci:** review fixes for the review gate ([2410539](https://github.com/harrymitchinson/oikade/commit/24105397ec65bde06a181aa231ecee5299d9ace8))
* **control:** review fixes for the claim arbiter ([2040e23](https://github.com/harrymitchinson/oikade/commit/2040e238f07e2bda8622af71ac954a5fd786d313))
* correct the Go module path to github.com/harrymitchinson/oikade ([#16](https://github.com/harrymitchinson/oikade/issues/16)) ([c6c3c50](https://github.com/harrymitchinson/oikade/commit/c6c3c50bf22f0a959d765767c3e1b5889ac7791a))
* **deploy:** review fixes for installing from a Release ([a1d8800](https://github.com/harrymitchinson/oikade/commit/a1d880001b835b782e51531aa6a0bc7c0c5bc75b))
* **device:** review fixes for control validation ([58ebe84](https://github.com/harrymitchinson/oikade/commit/58ebe848cae5abdd611e55e413ef83112979b486))
* **hack:** merge retries a queued review gate too ([#124](https://github.com/harrymitchinson/oikade/issues/124)) ([00ebd2b](https://github.com/harrymitchinson/oikade/commit/00ebd2bc113f26ae5814d47fb680e617b27d8930))
* **hack:** merge retries while the review gate its label started runs ([#121](https://github.com/harrymitchinson/oikade/issues/121)) ([f8e1b20](https://github.com/harrymitchinson/oikade/commit/f8e1b20d182cb293679779f71e689de755e370f8))
* **hack:** review fixes for agent writes ([74441a1](https://github.com/harrymitchinson/oikade/commit/74441a1a99e706147cf0ba016bf6322686e07d10))
* **hack:** review fixes for agent-token ([c29a669](https://github.com/harrymitchinson/oikade/commit/c29a66922c5201c610c8a798de31b053b019c255))
* **hack:** review fixes for the approved guard ([9b0aa74](https://github.com/harrymitchinson/oikade/commit/9b0aa749ccb9b2e8df4a8d9757ef33a2bc13afec))
* **hack:** satisfy the CI runner's shellcheck in plan claims ([22ebd2d](https://github.com/harrymitchinson/oikade/commit/22ebd2d9e3a2627f82684eb7463c631b6fc5a8d7))
* **hub:** keep broker waits out of the hub lock and skip status reconciles ([#10](https://github.com/harrymitchinson/oikade/issues/10)) ([f24b698](https://github.com/harrymitchinson/oikade/commit/f24b698df2cac12431740f02c310bef1bec47720))
* **plugins:** keep a set command's state over the initial one ([2b8690b](https://github.com/harrymitchinson/oikade/commit/2b8690ba17322e39bf525c5afe664e192f7829d7))
* **release:** review fixes for the release workflow ([bd86a01](https://github.com/harrymitchinson/oikade/commit/bd86a01794de9b8d59297881849a6fd56b1c6bed))
* **release:** review fixes for the version flag ([1a002a5](https://github.com/harrymitchinson/oikade/commit/1a002a5f7330570b157c6027970e0044bc21741f))
* **rule:** moments are triggers, not on ([7ca525b](https://github.com/harrymitchinson/oikade/commit/7ca525b29643078e34251b2abdd91cdb922059aa))
* **rule:** review fixes for arbitration in the runtime ([7613b6e](https://github.com/harrymitchinson/oikade/commit/7613b6e22976320894c6a4115b706a1ab855b304))
* **rule:** review fixes for holds and claims ([bf18d29](https://github.com/harrymitchinson/oikade/commit/bf18d29ab0d87b7dc16a04c09637e33747cb1972))
* **rule:** review fixes for holds in the runtime ([f331aec](https://github.com/harrymitchinson/oikade/commit/f331aecf038b975bfc56ea3850cbbec8b179229b))
* **rule:** review fixes for HomeKit writes as claims ([28b8962](https://github.com/harrymitchinson/oikade/commit/28b8962e18055711f7a72eb44c6a4f5b0a3fe822))
* **rule:** review fixes for Rules docs, e2e and dashboard ([3a2431f](https://github.com/harrymitchinson/oikade/commit/3a2431f47950b66ea5dced73da667255c36f51e1))
* **rule:** review fixes for the rename ([3e2f129](https://github.com/harrymitchinson/oikade/commit/3e2f129e54dad60b4f1f1f0ba426998dbaca021d))
* **rule:** review fixes for time and sun triggers ([15ceaa4](https://github.com/harrymitchinson/oikade/commit/15ceaa4672fdb151f35bb7bde5c0c2031d855ea8))
* **sdk:** review fixes for the Go plugin SDK ([6a39edd](https://github.com/harrymitchinson/oikade/commit/6a39edd3d95a947e5bd47d4569afb693804514c6))


### Performance Improvements

* **e2e:** build the manager image fast, with caching ([#127](https://github.com/harrymitchinson/oikade/issues/127)) ([117a44a](https://github.com/harrymitchinson/oikade/commit/117a44ac26b339e722342aae484e81dbf7ca4127))
* **e2e:** drop fmt/vet and cert-manager, and keep the local cluster ([#128](https://github.com/harrymitchinson/oikade/issues/128)) ([07e60ac](https://github.com/harrymitchinson/oikade/commit/07e60ac1fe68cd26fc360d775a643ec229e421b4))
* **e2e:** overlap the timetable spec's wait with the other specs ([#129](https://github.com/harrymitchinson/oikade/issues/129)) ([8414010](https://github.com/harrymitchinson/oikade/commit/8414010f664abc432789932b3bcfbac0172cfa99))
