# Changelog

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
