# Deploying Oikade on a Raspberry Pi

There are two ways to run Oikade, while the host replaces the operator ([OIK-143](https://linear.app/oikade/issue/OIK-143)). [One Pi, with the host](#one-pi-with-the-host) needs no cluster. The rest of this guide is the operator.

## One Pi, with the host

**What you need:** a Raspberry Pi 4 or 5 with a 64-bit OS, booting from an SSD if you can, on the same subnet as your iPhone and home hub, with IPv6 on. You run [Zigbee2MQTT](https://www.zigbee2mqtt.io) and an MQTT broker yourself, on this Pi or another. Your Mac needs Go, and `oikade`:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@main
```

**Install.** Use the name your SSH config has for the Pi. You need sudo there. The host runs as the user you log in as, which is what lets `oikade --host` reach it.

```sh
oikade install --host pi -- --broker mqtt://oikade@broker:1883 --broker-password-secret broker
```

If your broker needs no password, leave out the user and `--broker-password-secret`. If it does, set the secret and restart the host, as it can't connect until then:

```sh
oikade secret set broker --host pi
ssh pi sudo systemctl restart oikade
```

`oikade status --host pi` should answer. `journalctl -u oikade -f` on the Pi is its log, and `http://pi:8080/metrics` its metrics.

If that Pi runs a firewall, open Matter's port and mDNS: `sudo ufw allow 5540/udp` and `sudo ufw allow 5353/udp`.

**Your home.** Start a repo and deploy it:

```sh
oikade init home && cd home
oikade deploy --host pi
```

The [README](../README.md#getting-started-in-go) has the rest. `oikade matter --host pi` shows the code to pair with the Home app.

**Back up.** Your backup has two parts:

```sh
ssh pi oikade backup
rsync -a --rsync-path="sudo rsync" pi:/var/lib/oikade/ pi-backup/oikade/          # everything Oikade has
```

The second part is Zigbee2MQTT's own data directory, wherever you keep it, which holds the network key and the database. It isn't Oikade's, and Oikade's backup doesn't have it. Losing it means pairing every Zigbee device again.

The copy holds your secrets and the bridge's keys in the clear, so keep it private.

**Restore to a new Pi.** The same address and subnet keep the Home app happy, and nothing needs pairing again:

```sh
oikade install --host new-pi -- --broker mqtt://oikade@broker:1883 --broker-password-secret broker
ssh new-pi sudo systemctl stop oikade
rsync -a --rsync-path="sudo rsync" pi-backup/oikade/ new-pi:/var/lib/oikade/
ssh new-pi 'cd /var/lib/oikade && sudo cp "$(sudo ls -d backups/* | tail -1)" oikade.db && sudo chown -R "$USER": . && sudo systemctl start oikade'
```

The `cp` puts the snapshot where the store goes, since the `oikade.db` in the copy was changing as it was copied. A controller paired since that backup isn't in it, and needs pairing again.

**Upgrade.** Update `oikade` on your Mac, then:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@main
oikade upgrade --host pi
```

That builds this version for the Pi, backs up the store, swaps the binary and restarts the host. Matter stops with it, so the Home app shows no response until it's back, and the command says how long that took. If the new version doesn't come up, the old one goes back. Afterwards, update `oikade` in your home's `go.mod` and run `oikade deploy --host pi`, as your code is built against the old version.

## A cluster, with the operator

This guide takes you from blank Pis to a paired Matter bridge, Zigbee devices declared in Git, and dashboards in Grafana. It assumes k3s, but any distribution works as long as you keep the networking requirements in step 3.

**Contents**

0. [One Pi, with the host](#one-pi-with-the-host)
1. [What you need](#1-what-you-need)
2. [Prepare the Pis](#2-prepare-the-pis)
3. [Install k3s](#3-install-k3s)
4. [Choose nodes for Matter and Zigbee](#4-choose-nodes-for-matter-and-zigbee)
5. [Storage](#5-storage)
6. [Choose a release](#6-choose-a-release)
7. [Install Oikade](#7-install-oikade)
8. [Set up a Git repo for your home](#8-set-up-a-git-repo-for-your-home)
9. [Run Zigbee2MQTT](#9-run-zigbee2mqtt)
10. [Pair and declare Zigbee devices](#10-pair-and-declare-zigbee-devices)
11. [Run plugins](#11-run-plugins)
12. [Pair the Matter bridge](#12-pair-the-matter-bridge)
13. [Add rules](#13-add-rules)
14. [Monitoring](#14-monitoring)
15. [Back up what Git can't rebuild](#15-back-up-what-git-cant-rebuild)
16. [Upgrades](#16-upgrades)
17. [Troubleshooting](#17-troubleshooting)

---

## 1. What you need

- **Raspberry Pi 4 or 5** (2 GB is enough; 4 GB gives room for Prometheus and Grafana), running a **64-bit** OS. Oikade's images are `linux/arm64` only on the Pi.
- **Boot from SSD if you can.** Prometheus and the Zigbee database write constantly, and SD cards wear out.
- **A Zigbee coordinator.** A network coordinator such as the SMLIGHT SLZB-06 is the easiest choice: it sits on Ethernet, so the Zigbee2MQTT pod can run on any Pi. A USB stick such as the Sonoff ZBDongle-E works too, but ties the pod to the Pi it's plugged into.
- **A container registry the Pis can pull from.** The examples use GitHub Container Registry (`ghcr.io`).
- **Wired, or at least reliable, networking**, on the same network and subnet as your iPhone. See step 4.

## 2. Prepare the Pis

Flash **Raspberry Pi OS Lite (64-bit)** with Raspberry Pi Imager. In its settings, set a unique hostname for each Pi (`pi-1`, `pi-2`, …), enable SSH, and add your key.

On every Pi:

```sh
# Give each Pi a fixed address: a DHCP reservation on your router is simplest.

# k3s needs the memory cgroup, which Raspberry Pi OS leaves off.
sudo sed -i '1 s/$/ cgroup_memory=1 cgroup_enable=memory/' /boot/firmware/cmdline.txt

sudo apt update && sudo apt full-upgrade -y
sudo reboot
```

After the reboot, check that the cgroup is on:

```sh
grep memory /proc/cgroups    # the last column should be 1
```

## 3. Install k3s

On the first Pi (the server):

```sh
curl -sfL https://get.k3s.io | sh -s - --write-kubeconfig-mode 644 --disable traefik
sudo cat /var/lib/rancher/k3s/server/node-token     # needed for the other Pis
```

Traefik is disabled because nothing in Oikade needs an ingress. Leave it on if you use it for other things.

On each other Pi (agents):

```sh
curl -sfL https://get.k3s.io | K3S_URL=https://pi-1:6443 K3S_TOKEN=<node-token> sh -
```

On your Mac, copy the kubeconfig over and point it at the server:

```sh
scp pi-1:/etc/rancher/k3s/k3s.yaml ~/.kube/pi.yaml
sed -i '' 's/127.0.0.1/pi-1/' ~/.kube/pi.yaml
export KUBECONFIG=~/.kube/pi.yaml
kubectl get nodes -o wide
```

## 4. Choose nodes for Matter and Zigbee

**Matter.** The Matter bridge pod uses the host network, because controllers find it over mDNS, which does not cross subnets or VLANs, and reach it over IPv6. Choose a Pi that is on the same subnet as your iPhone and your home hub (an Apple TV or HomePod), with IPv6 enabled (Raspberry Pi OS has it on by default), and label it:

```sh
kubectl label node pi-1 oikade.dev/matter=true
```

If that Pi runs a firewall (Raspberry Pi OS doesn't by default), open the Matter port pool and mDNS:

```sh
sudo ufw allow 5540:5640/udp
sudo ufw allow 5353/udp
```

**Zigbee.** A network coordinator needs nothing here. For a USB stick, label the Pi it's plugged into and find the device's stable path:

```sh
kubectl label node pi-2 oikade.dev/zigbee=true
ssh pi-2 ls -l /dev/serial/by-id/      # e.g. usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_...-if00-port0
```

Use the `/dev/serial/by-id/...` path rather than `/dev/ttyUSB0`, which can change between reboots.

## 5. Storage

k3s ships with the `local-path` StorageClass. It's the default and it works, but each PersistentVolumeClaim then lives on one Pi's disk, under `/var/lib/rancher/k3s/storage`. If that Pi dies, you lose:

- the Matter fabrics (the `<bridge>-matter` PVC), which means commissioning the bridge again, and the Home app forgets every room, scene and automation;
- the Zigbee database (the `<instance>-data` PVC), which zigbee2mqtt can rebuild from the coordinator's own backup, though devices may need re-interviewing.

You have two options:

- **Keep `local-path` and take backups** as in step 15. This is fine for a single-node or small cluster.
- **Use replicated storage** such as [Longhorn](https://longhorn.io). It needs `open-iscsi` on every Pi (`sudo apt install -y open-iscsi`) and adds noticeable load on a Pi 4.

## 6. Choose a release

Pick a version from [Releases](https://github.com/harrymitchinson/oikade/releases). Each one has an `install.yaml` with every manifest Oikade needs, and an image at `ghcr.io/harrymitchinson/oikade:<version>` for arm64 and amd64. The same image runs the operator, the runtime and the Matter bridge pods. The image and `install.yaml` are added after the release's tests pass, about fifteen minutes after it appears. Pick a release that lists `install.yaml` under its assets.

Milestones such as `v0.1.0` are full releases. Versions between them are pre-releases of the next one, such as `v0.2.0-alpha.1`. Any of them may change the API, so read the release notes before you upgrade (README, "Versioning").

<details>
<summary>Building from source instead</summary>

On your Mac, in this repo:

```sh
echo <github-token> | docker login ghcr.io -u <github-user> --password-stdin
TAG=v0.0.0-$(git rev-parse --short HEAD)   # a new tag per build, so nodes pull it
make docker-buildx IMG=ghcr.io/<github-user>/oikade:$TAG VERSION=$TAG PLATFORMS=linux/arm64
make build-installer IMG=ghcr.io/<github-user>/oikade:$TAG
```

Copy `dist/install.yaml` into the `oikade/` directory of step 7, and list it as `- install.yaml` in place of the Release's URL. For monitoring (step 14), use `config/prometheus` and `config/grafana` from the same checkout. `make build-installer` writes the image name into `config/manager/kustomization.yaml` and `config/runtime/kustomization.yaml`, so revert those two files afterwards. Either make your package public in GitHub's package settings, or add an `imagePullSecret` to the `oikade-system` service accounts.

</details>

## 7. Install Oikade

The Release's `install.yaml` runs the runtime with `--timezone=UTC` and no location. Rules see the timezone as `tz`, and time triggers use it. Sun triggers need your latitude and longitude, and without them they don't fire. Patch all three in from a directory of your own, which becomes part of your home repo in step 8:

`oikade/kustomization.yaml`:

```yaml
resources:
- https://github.com/harrymitchinson/oikade/releases/download/v0.1.0/install.yaml
patches:
# Appends to the runtime's flags. A second --timezone wins over the UTC default.
- target: {kind: Deployment, name: oikade-runtime}
  patch: |
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --timezone=Europe/London    # your IANA timezone
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --latitude=51.5074          # north positive
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --longitude=-0.1278         # east positive
```

Then apply it:

```sh
kubectl apply -k oikade/
kubectl -n oikade-system get pods
```

You should see three pods running: `oikade-controller-manager`, `oikade-runtime` and `oikade-mosquitto`.

> **Security:** Mosquitto allows anonymous connections from inside the cluster, so anything that can reach `oikade-mosquitto:1883` can control your devices. At minimum, keep untrusted workloads off this cluster or add a NetworkPolicy.

## 8. Set up a Git repo for your home

Keep your home's manifests in their own repo, separate from Oikade's code:

```
home-infra/
├── clusters/pi/              # Flux entry point
│   ├── oikade.yaml           # Kustomization pointing at ./oikade
│   └── home.yaml             # Kustomization pointing at ./home
├── oikade/
│   └── kustomization.yaml    # step 7: the Release's install.yaml and your timezone
└── home/
    ├── kustomization.yaml
    ├── namespace.yaml
    ├── rooms.yaml
    ├── zigbee2mqtt.yaml
    ├── devices/
    │   ├── hallway.yaml
    │   └── kitchen.yaml
    ├── bridge.yaml
    ├── plugins/              # step 11, if you have non-Zigbee devices
    │   ├── sonoff.yaml
    │   └── sonoff-keys.yaml  # encrypted with SOPS
    └── rules/
        └── hallway-motion-light.yaml
```

`home/kustomization.yaml`:

```yaml
namespace: home
resources:
- namespace.yaml
- rooms.yaml
- zigbee2mqtt.yaml
- devices/hallway.yaml
- bridge.yaml
- rules/hallway-motion-light.yaml
```

`config/samples` in this repo has a starting point for every file.

The Matter bridge's setup code isn't in Git: the operator generates it into the bridge's Secret (step 12).

```sh
kubectl create namespace home
```

To reconcile the repo with Flux:

```sh
brew install fluxcd/tap/flux
flux bootstrap github --owner=<github-user> --repository=home-infra --path=clusters/pi --personal
```

`clusters/pi/oikade.yaml` installs Oikade itself, from the Release named in `oikade/kustomization.yaml`. It has `prune: false` because it owns Oikade's CRDs. If Flux pruned them, say after this file was renamed or a URL failed to build, every Device, Rule and Bridge would be deleted with them, and the accessory IDs kept in each Bridge's status with them, so the Home app would forget your rooms, scenes and automations.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: oikade
  namespace: flux-system
spec:
  interval: 1h
  path: ./oikade
  prune: false
  wait: true
  sourceRef:
    kind: GitRepository
    name: flux-system
```

`clusters/pi/home.yaml` applies your home once Oikade's CRDs are in place:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: home
  namespace: flux-system
spec:
  dependsOn:
  - name: oikade
  interval: 5m
  path: ./home
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
```

From here on, changing your home means opening a PR. Until you've set up Flux, `kubectl apply -k home/` does the same job.

## 9. Run Zigbee2MQTT

`home/zigbee2mqtt.yaml` for a network coordinator:

```yaml
apiVersion: zigbee.oikade.dev/v1alpha1
kind: Zigbee2MQTT
metadata:
  name: z2m
spec:
  serial:
    port: tcp://192.168.1.50:6638     # your coordinator's address
    adapter: ember                    # zstack for TI chips, ember for Silicon Labs (SLZB-06M, ZBDongle-E)
  frontend: true                      # handy while pairing
```

For a USB stick, use its stable path and pin the pod to that Pi:

```yaml
spec:
  serial:
    port: /dev/serial/by-id/usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_...-if00-port0
    adapter: ember
  nodeSelector:
    oikade.dev/zigbee: "true"
```

Apply it and check that it starts:

```sh
kubectl -n home get zigbee2mqtt z2m              # READY should become True
kubectl -n home logs deploy/z2m -f               # look for "Zigbee2MQTT started"
```

The operator generates the Zigbee network key, PAN ID and extended PAN ID once, and stores them in the `z2m-network` Secret. **Back up that Secret now** (step 15). If it's lost, every device has to be paired again.

## 10. Pair and declare Zigbee devices

1. Open the frontend:
   ```sh
   kubectl -n home port-forward svc/z2m 8080
   ```
   Go to http://localhost:8080 and click **Permit join (All)**.
2. Put the device in pairing mode (see its manual) and wait for it to appear in the frontend.
3. Note its **IEEE address** (`0x...`). It now counts as undeclared, and the runtime logs say so:
   ```sh
   kubectl -n oikade-system logs deploy/oikade-runtime | grep "without a Device"
   ```
4. Declare it in Git, e.g. in `home/devices/hallway.yaml`:
   ```yaml
   apiVersion: home.oikade.dev/v1alpha1
   kind: Device
   metadata:
     name: hallway-light          # used by Rules, Matter and Grafana; Zigbee2MQTT keeps its own name
   spec:
     displayName: Hallway Light   # shown in the Home app and Grafana
     roomRef: {name: hallway}
     zigbee2mqtt:
       instanceRef: {name: z2m}
       ieeeAddress: "0x000b57fffe000001"
   ```
   Smart plugs report themselves as switches, which the Matter bridge shows as a plug-in unit (an outlet). Add `matter: {deviceType: Light}` or `matter: {deviceType: MountedControl}` to show one as a light or a wall switch.
5. Commit, then check that the device resolves:
   ```sh
   kubectl -n home get devices
   ```
   `Resolved=True` and a list of capabilities means it worked. If `Resolved=False` with reason `NotPaired`, the IEEE address is wrong. If a capability you expected is missing, Oikade doesn't map that feature yet (see "Not yet" in the README).

Turn **Permit join** off again when you're done. Settings changed in the Zigbee2MQTT frontend are overwritten from Git when the pod restarts, so make them in Git. Device names are the exception: they're Zigbee2MQTT's own, they survive restarts, and Oikade ignores them.

## 11. Run plugins

Devices that aren't Zigbee, such as SONOFF LAN relays, are served by a [plugin](../README.md#plugins): a separate pod that speaks Oikade's capabilities over MQTT. Skip this step if you only have Zigbee devices.

Each plugin is released from its own repo, `oikade-plugin-<name>`, as an image for linux/arm64 and linux/amd64 at `ghcr.io/<github-user>/oikade-plugin-<name>:<tag>`. Use a released tag. Oikade doesn't build plugin images.

[`oikade-plugin-example-switch`](https://github.com/harrymitchinson/oikade-plugin-example-switch) simulates relays, so you can try the whole path, from the Home app to Grafana, before buying hardware. Its README has a Plugin and a Device to apply, and its releases list the image tags. The rest of this step shows a plugin that needs per-device keys and mDNS, using a SONOFF LAN plugin as the example; `example-switch` needs neither, so for it, leave out `hostNetwork`, `secretRef` and the Secret.

**Credentials.** A plugin that needs per-device keys reads them from a Secret named in `spec.secretRef`, with one key per device id. The operator mounts it but never creates, changes or deletes it, so it's yours to keep in Git. Encrypt it with SOPS and let Flux decrypt it:

```sh
brew install sops age
age-keygen -o age.agekey
kubectl -n flux-system create secret generic sops-age --from-file=age.agekey=age.agekey
# Keep age.agekey somewhere safe, such as a password manager: without it the file can't be decrypted.

kubectl -n home create secret generic sonoff-keys --dry-run=client -o yaml \
  --from-literal=1000aabbcc=<lan-key> > home/plugins/sonoff-keys.yaml
sops --encrypt --age <age-public-key> --encrypted-regex '^(data|stringData)$' \
  --in-place home/plugins/sonoff-keys.yaml
```

Then add `decryption: {provider: sops, secretRef: {name: sops-age}}` to the `spec` of `clusters/pi/home.yaml`.

`home/plugins/sonoff.yaml`:

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata:
  name: sonoff
spec:
  image: ghcr.io/<github-user>/oikade-plugin-sonoff:v0.1.0
  hostNetwork: true                # see below
  secretRef: {name: sonoff-keys}
  nodeSelector: {oikade.dev/devices-lan: "true"}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata:
  name: porch-light
spec:
  roomRef: {name: hallway}
  plugin:
    pluginRef: {name: sonoff}
    id: 1000aabbcc
    parameters: {capability: light.on}   # relays report switch.on unless told otherwise
```

**`hostNetwork`.** Plugins that find devices with mDNS or multicast, such as SONOFF LAN, must run on the host network, like the Matter bridge pod. They then need a node on the same L2 network as the devices, as the Matter bridge pod does with your iPhone (step 4). Label one, and pin the plugin to it with `nodeSelector` as above:

```sh
kubectl label node pi-1 oikade.dev/devices-lan=true
```

Plugins that only make outbound connections, such as cloud APIs, don't need either.

Add `plugins/sonoff-keys.yaml` and `plugins/sonoff.yaml` to the `resources` in `home/kustomization.yaml`, then check the plugin:

```sh
kubectl -n home get plugins                     # READY True once the pod runs
kubectl -n home get device porch-light \
  -o jsonpath='{.status.conditions[?(@.type=="Resolved")]}'   # PluginError explains a bad parameter or missing key
kubectl -n home logs deploy/sonoff-plugin
```

`oikade_plugin_up{plugin="sonoff"}` is 1 while the plugin is connected. If it crashes, the broker marks it offline and its devices show "No Response" in the Home app until Kubernetes restarts it.

## 12. Pair the Matter bridge

`home/bridge.yaml`:

```yaml
apiVersion: matter.oikade.dev/v1alpha1
kind: Bridge
metadata:
  name: main
spec:
  displayName: Oikade
  deviceSelector: {}                 # every Device; narrow it with matchLabels if needed
  nodeSelector:
    oikade.dev/matter: "true"
```

Apply it and check:

```sh
kubectl -n home get bridge main                    # READY True, PORT 5540
kubectl -n home logs deploy/main-matter            # "Advertised commissionable service", "Added bridged device"
```

The operator generates the setup code into the `main-matter` Secret. Read the 11-digit pairing code from it:

```sh
kubectl -n home get secret main-matter -o jsonpath='{.data.manual-code}' | base64 -d
```

Check that the bridge is advertised. On your Mac, on the same network:

```sh
dns-sd -B _matterc._udp
```

A commissionable node should appear. The bridge accepts commissioning for 15 minutes after it starts; if that has passed, `kubectl -n home rollout restart deploy/main-matter`. Then on your iPhone:

1. In the Home app, tap **+**, then **Add Accessory**, then **More options…**.
2. Choose **Oikade** and enter the pairing code. The Home app warns that it's an uncertified accessory: choose **Add Anyway**.
3. Assign each accessory to a room. The Home app doesn't take rooms from a bridge, so you do this once there.

From now on:

- **Adding or removing a Device** updates the bridge automatically, and the Home app picks up the change.
- **Endpoint IDs never change**, so rooms, scenes and favourites survive restarts, and even deleting and re-creating a Device.
- **Restarting the bridge pod** (`kubectl -n home rollout restart deploy/main-matter`) keeps the pairing.
- `kubectl -n home get bridge main -o yaml` lists the controllers it's paired with under `status.fabrics`.

## 13. Add rules

`home/rules/hallway-motion-light.yaml`:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata:
  name: hallway-motion-light
spec:
  mode: Restart
  triggers:
  - change: {device: hallway-motion, capability: sensor.occupancy-detected}
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  do:
  - set: {device: hallway-light, capability: light.on, value: true}
  - delay: 5m
  - set: {device: hallway-light, capability: light.on, value: false}
```

A hold keeps something on for as long as a window or a condition holds, and stays right across a restart. `home/rules/water-heater-night.yaml`:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata:
  name: water-heater-night
spec:
  while:
    window: {start: "22:00", end: "06:00"}
  hold: {device: water-heater, capability: switch.on, value: true}
```

```sh
kubectl -n home get rules           # READY False explains what's wrong; ACTIVE shows which holds are on
```

Sun triggers and windows need the `--latitude` and `--longitude` from step 7. The README's [Rules](../README.md#rules) section covers the CEL variables, holds, Safety rules, and arbitration: what happens when rules, the Home app and a wall switch disagree.

## 14. Monitoring

Install the kube-prometheus-stack chart. Both `--set` flags matter: without the first, Prometheus ignores Oikade's ServiceMonitor; the second makes sure Grafana's dashboard sidecar also looks in `oikade-system`.

```sh
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install kps prometheus-community/kube-prometheus-stack -n monitoring --create-namespace \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
  --set grafana.sidecar.dashboards.searchNamespace=ALL \
  --set prometheus.prometheusSpec.retention=90d
```

The ServiceMonitors and the dashboard aren't in `install.yaml`, so add them from the same release. `oikade/monitoring/kustomization.yaml`:

```yaml
namespace: oikade-system
namePrefix: oikade-
resources:
- https://github.com/harrymitchinson/oikade//config/prometheus?ref=v0.1.0
- https://github.com/harrymitchinson/oikade//config/grafana?ref=v0.1.0
```

Add `- monitoring` to the `resources` in `oikade/kustomization.yaml`, and keep its version in step with the `install.yaml` URL when you upgrade. Then run `kubectl apply -k oikade/`, or commit it for Flux.

```sh
kubectl -n monitoring get secret kps-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl -n monitoring port-forward svc/kps-grafana 3000:80
```

The **Oikade home** dashboard shows temperature and humidity per room, light and door timelines, motion events, battery levels, availability, rule runs, active holds, their next changes, and which level wins each arbitrated capability. On a Pi 4, consider a Prometheus memory limit and an SSD for the Prometheus PVC.

## 15. Back up what Git can't rebuild

Everything in your home repo can be rebuilt from Git. These can't:

| What | Where | If you lose it |
|---|---|---|
| Zigbee network identity | Secret `home/z2m-network` | Every Zigbee device must be paired again |
| Matter bridge setup code, unique ID and endpoint IDs | Secret `home/main-matter` | The Home app sees a new bridge |
| Matter fabrics and keys | PVC `home/main-matter` | You must pair again, and the Home app forgets rooms, scenes and automations |
| Zigbee database and coordinator backup | PVC `home/z2m-data` | zigbee2mqtt rebuilds what it can; devices may need re-interviewing |
| The runtime's state: active holds, latches, claims and limit counters | PVC `oikade-system/oikade-mosquitto`, as retained MQTT messages | Holds are worked out again from the clock, but latched Safety holds whose condition has passed are released, overrides end early, and equipment limits start counting again |
| Plugin credentials | Your `spec.secretRef` Secrets, such as `home/sonoff-keys` | The plugin reports an error for every device. If they're in Git with SOPS, they come back with Flux. |
| SOPS age key | `age.agekey`, and Secret `flux-system/sops-age` | Nothing encrypted in Git can be decrypted. Keep `age.agekey` in a password manager. |

Export the Secrets now, and again whenever you create a new Bridge or Zigbee2MQTT instance. Store them somewhere safe, such as a password manager. Don't put them in Git unencrypted.

```sh
kubectl -n home get secret z2m-network main-matter -o yaml > oikade-secrets-backup.yaml
kubectl -n flux-system get secret sops-age -o yaml >> oikade-secrets-backup.yaml   # if you use SOPS
```

For the PVCs on `local-path`, copy the directories off the Pi they live on. Find which Pi holds each one, then list its directories, which are named `pvc-<uid>_<namespace>_<claim>`:

```sh
kubectl -n home get pod -o wide                       # main-matter and z2m pods show their node
kubectl -n oikade-system get pod -o wide              # and oikade-mosquitto
ssh pi-1 sudo ls /var/lib/rancher/k3s/storage/        # e.g. pvc-3f2..._home_main-matter
```

Copy them to another machine from a nightly cron, e.g. `rsync -a --rsync-path="sudo rsync" pi-1:/var/lib/rancher/k3s/storage/pvc-3f2..._home_main-matter/ backups/main-matter/`. If you use Longhorn, use its scheduled backups to NFS or S3 instead.

**To restore:** apply the Secrets *before* the Bridge and Zigbee2MQTT objects. The operator never overwrites existing ones, so it picks them up. If you use SOPS, create `flux-system/sops-age` before Flux reconciles `home`: until it exists, Flux can't decrypt anything in the tree and applies none of it.

## 16. Upgrades

Read the release notes, then change the version in the URL in `oikade/kustomization.yaml`:

```yaml
resources:
- https://github.com/harrymitchinson/oikade/releases/download/v0.2.0-alpha.1/install.yaml
```

If you added monitoring (step 14), change `?ref=` in `oikade/monitoring/kustomization.yaml` to the same version. Commit it and let Flux apply it, or run `kubectl apply -k oikade/`. Your timezone and location patch carries over.

The operator and runtime roll over. Matter bridge pods pick up the new image the next time the operator applies them, keeping their pairings. They use the Recreate strategy, so expect a few seconds when the Home app shows "No Response".

To upgrade Zigbee2MQTT, set `spec.image` on the Zigbee2MQTT object, or change the operator's `--zigbee2mqtt-image` default.

Plugins are versioned separately. Build and push the new plugin image (step 11), then change `spec.image` on the Plugin. Upgrading Oikade doesn't touch them.

## 17. Troubleshooting

| Symptom | Check |
|---|---|
| Bridge doesn't appear in the Home app | `dns-sd -B _matterc._udp` from a Mac on the same network. If it's missing: has the 15-minute window passed (`Closed the commissioning window` in `kubectl -n home logs deploy/main-matter`)? Is the pod on the labelled node (`kubectl -n home get pod -o wide`)? Same subnet as the iPhone and home hub? Is UDP 5353 open? Does the router or access point block multicast between Wi-Fi and Ethernet? |
| "Accessory not found" or pairing fails | `kubectl -n home logs deploy/main-matter`. Is IPv6 on for the Pi and the network? Is there a home hub? |
| Device stays `Resolved=False` (`NotPaired`) | The IEEE address in Git doesn't match the paired device. Compare with the Zigbee2MQTT frontend. |
| Device stays `Resolved=False` (`NoDriver`) | `instanceRef` or `pluginRef` names a Zigbee2MQTT or Plugin that doesn't exist, or another Device already claims that IEEE address or plugin id (see the Zigbee2MQTT or Plugin `Ready` condition). |
| Device stays `Resolved=False` (`PluginError`) | The plugin rejected the Device's `parameters` or has no key for its id. The condition message says which. |
| Plugin isn't `READY` | `kubectl -n home get plugin <name> -o yaml` shows the reason: `SecretNotFound`, `DuplicateDevice`, `NameConflict`, or `Progressing` while the pod starts. Check `kubectl -n home logs deploy/<name>-plugin`. |
| A capability is missing | Oikade doesn't map that expose yet, e.g. colour, colour temperature or multi-gang switches (`state_l2`). |
| The Home app shows "No Response" | Is the device `Available=False` in `kubectl get devices`? Does `kubectl -n home logs deploy/main-matter` show `Failed to set capability from Matter`? |
| Rule never fires | Is it `READY` in `kubectl get rules`? `oikade_rule_runs_total` shows whether it's hitting `condition_false`. Check `--timezone` if the condition uses the time. |
| zigbee2mqtt crash-loops | `kubectl -n home logs deploy/z2m`. Check the coordinator address or USB path, and that `adapter` matches the coordinator's chip. |
| Something is wrong with MQTT | Watch the bus: `kubectl -n home run mqtt --rm -it --image=eclipse-mosquitto:2.1.2-alpine -- mosquitto_sub -h oikade-mosquitto.oikade-system.svc -t '#' -v` |
