# Deploying Oikade on a Raspberry Pi cluster

This guide takes you from blank Pis to a paired HomeKit bridge, Zigbee devices declared in Git, and dashboards in Grafana. It assumes k3s, but any distribution works as long as you keep the networking requirements in step 3.

**Contents**

1. [What you need](#1-what-you-need)
2. [Prepare the Pis](#2-prepare-the-pis)
3. [Install k3s](#3-install-k3s)
4. [Choose nodes for HomeKit and Zigbee](#4-choose-nodes-for-homekit-and-zigbee)
5. [Storage](#5-storage)
6. [Build and push the image](#6-build-and-push-the-image)
7. [Install Oikade](#7-install-oikade)
8. [Set up a Git repo for your home](#8-set-up-a-git-repo-for-your-home)
9. [Run Zigbee2MQTT](#9-run-zigbee2mqtt)
10. [Pair and declare Zigbee devices](#10-pair-and-declare-zigbee-devices)
11. [Run plugins](#11-run-plugins)
12. [Pair the HomeKit bridge](#12-pair-the-homekit-bridge)
13. [Add automations](#13-add-automations)
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

## 4. Choose nodes for HomeKit and Zigbee

**HomeKit.** The HAP pod uses the host network, because HomeKit finds bridges over mDNS, and mDNS does not cross subnets or VLANs. Choose a Pi that is on the same subnet as your iPhone and label it:

```sh
kubectl label node pi-1 oikade.dev/homekit=true
```

If that Pi runs a firewall (Raspberry Pi OS doesn't by default), open the HAP port pool and mDNS:

```sh
sudo ufw allow 51830:51930/tcp
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

- the HomeKit pairing (the `<bridge>-hap` PVC), which means re-pairing, and the Home app forgets every room, scene and automation;
- the Zigbee database (the `<instance>-data` PVC), which zigbee2mqtt can rebuild from the coordinator's own backup, though devices may need re-interviewing.

You have two options:

- **Keep `local-path` and take backups** as in step 15. This is fine for a single-node or small cluster.
- **Use replicated storage** such as [Longhorn](https://longhorn.io). It needs `open-iscsi` on every Pi (`sudo apt install -y open-iscsi`) and adds noticeable load on a Pi 4.

## 6. Build and push the image

On your Mac, in this repo:

```sh
echo <github-token> | docker login ghcr.io -u <github-user> --password-stdin

make docker-buildx IMG=ghcr.io/<github-user>/oikade:v0.1.0 PLATFORMS=linux/arm64
```

The same image runs the operator, the runtime and the HAP pods. Either make the package public in GitHub's package settings, or add an `imagePullSecret` to the `oikade-system` service accounts.

## 7. Install Oikade

First, set your timezone so automations see local time. Edit `config/runtime/deployment.yaml`:

```yaml
        - --timezone=Europe/London    # your IANA timezone
```

Then render a single install file and apply it:

```sh
make build-installer IMG=ghcr.io/<github-user>/oikade:v0.1.0
kubectl apply -f dist/install.yaml
kubectl -n oikade-system get pods
```

You should see three pods running: `oikade-controller-manager`, `oikade-runtime` and `oikade-mosquitto`.

`make build-installer` writes the image name into `config/manager/kustomization.yaml` and `config/runtime/kustomization.yaml`. Commit `dist/install.yaml` if you want Flux to install Oikade itself (step 8). Revert the two kustomizations if you don't want the tag in the repo.

> **Security:** Mosquitto allows anonymous connections from inside the cluster, so anything that can reach `oikade-mosquitto:1883` can control your devices. At minimum, keep untrusted workloads off this cluster or add a NetworkPolicy.

## 8. Set up a Git repo for your home

Keep your home's manifests in their own repo, separate from Oikade's code:

```
home-infra/
├── clusters/pi/              # Flux entry point
│   └── home.yaml             # Kustomization pointing at ./home
└── home/
    ├── kustomization.yaml
    ├── namespace.yaml
    ├── rooms.yaml
    ├── zigbee2mqtt.yaml
    ├── devices/
    │   ├── hallway.yaml
    │   └── kitchen.yaml
    ├── bridge.yaml
    └── automations/
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
- automations/hallway-motion-light.yaml
```

`config/samples` in this repo has a starting point for every file.

**Don't commit the HomeKit setup code in plain text.** Create that Secret by hand:

```sh
kubectl create namespace home
kubectl -n home create secret generic homekit-pin --from-literal=pin=482-91-736   # choose your own
```

Alternatively, encrypt it in the repo with [SOPS](https://fluxcd.io/flux/guides/mozilla-sops/) or [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets).

To reconcile the repo with Flux:

```sh
brew install fluxcd/tap/flux
flux bootstrap github --owner=<github-user> --repository=home-infra --path=clusters/pi --personal
```

`clusters/pi/home.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: home
  namespace: flux-system
spec:
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
     name: hallway-light          # becomes the device's Zigbee2MQTT friendly name
   spec:
     displayName: Hallway Light   # shown in HomeKit and Grafana
     roomRef: {name: hallway}
     zigbee2mqtt:
       instanceRef: {name: z2m}
       ieeeAddress: "0x000b57fffe000001"
   ```
   Smart plugs report themselves as switches. Add `homekit: {service: Outlet}` to show one as an outlet.
5. Commit, then check that the device resolves:
   ```sh
   kubectl -n home get devices
   ```
   `Resolved=True` and a list of capabilities means it worked. If `Resolved=False` with reason `NotPaired`, the IEEE address is wrong. If a capability you expected is missing, Oikade doesn't map that feature yet (see "Not yet" in the README).

Turn **Permit join** off again when you're done. Changes made in the Zigbee2MQTT frontend, such as renames, are overwritten from Git when the pod restarts, so make them in Git.

## 11. Run plugins

Devices that aren't Zigbee, such as SONOFF LAN relays, are served by a [plugin](../README.md#plugins): a separate pod that speaks Oikade's capabilities over MQTT. Skip this step if you only have Zigbee devices.

Plugin images must be arm64. Build one from this repo, the same way as the Oikade image:

```sh
make docker-buildx-plugin PLUGIN=example-switch \
  PLUGIN_IMG=ghcr.io/<github-user>/oikade-plugin-example-switch:v0.1.0 \
  PLUGIN_PLATFORMS=linux/arm64 PLUGIN_BUILD_ARGS=--push
```

`example-switch` simulates relays, so you can try the whole path, from the Home app to Grafana, before buying hardware.

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
  nodeSelector: {kubernetes.io/arch: arm64}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata:
  name: porch-light
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: sonoff}
    id: 1000aabbcc
    parameters: {capability: light.on}   # relays report switch.on unless told otherwise
```

**`hostNetwork`.** Plugins that find devices with mDNS or multicast, such as SONOFF LAN, must run on the host network, like the HAP pod. They then need a node on the same L2 network as the devices (step 4). Plugins that only make outbound connections, such as cloud APIs, don't need it.

Add both files to `home/kustomization.yaml`, then check the plugin:

```sh
kubectl -n home get plugins                     # READY True once the pod runs
kubectl -n home get devices porch-light         # Resolved=False PluginError explains a bad parameter or missing key
kubectl -n home logs deploy/sonoff-plugin
```

`oikade_plugin_up{plugin="sonoff"}` is 1 while the plugin is connected. If it crashes, the broker marks it offline and its devices show "No Response" in HomeKit until Kubernetes restarts it.

## 12. Pair the HomeKit bridge

`home/bridge.yaml`:

```yaml
apiVersion: homekit.oikade.dev/v1alpha1
kind: Bridge
metadata:
  name: main
spec:
  displayName: Oikade
  pinSecretRef: {name: homekit-pin, key: pin}
  nodeSelector:
    oikade.dev/homekit: "true"
  # deviceSelector: {matchLabels: {homekit: "true"}}   # optional: expose only some Devices
```

Apply it and check:

```sh
kubectl -n home get bridge main                    # READY True, PORT 51830
kubectl -n home logs deploy/main-hap               # "Starting HAP server" with the accessory count
```

Check that the bridge is advertised. On your Mac, on the same network:

```sh
dns-sd -B _hap._tcp
```

`Oikade` should appear in the list. Then on your iPhone:

1. In the Home app, tap **+**, then **Add Accessory**, then **More options…**.
2. Choose **Oikade** and enter the setup code from your `homekit-pin` Secret.
3. Assign each accessory to a room. HomeKit can't receive rooms from a bridge, so you do this once in the app.

From now on:

- **Adding or removing a Device** updates the bridge automatically, and the Home app picks up the change.
- **Accessory IDs never change**, so rooms, scenes and favourites survive restarts, and even deleting and re-creating a Device.
- **Restarting the HAP pod** (`kubectl -n home rollout restart deploy/main-hap`) keeps the pairing.

## 13. Add automations

`home/automations/hallway-motion-light.yaml`:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Automation
metadata:
  name: hallway-motion-light
spec:
  mode: Restart
  triggers:
  - {device: hallway-motion, capability: sensor.occupancy-detected}
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  actions:
  - set: {device: hallway-light, capability: light.on, value: true}
  - delay: 5m
  - set: {device: hallway-light, capability: light.on, value: false}
```

```sh
kubectl -n home get automations     # READY False explains what's wrong with the condition or actions
```

The README describes the CEL variables and the capability names.

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

In this repo, uncomment `- ../prometheus` and `- ../grafana` in `config/default/kustomization.yaml`. Then run `make build-installer` again and re-apply `dist/install.yaml`.

```sh
kubectl -n monitoring get secret kps-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl -n monitoring port-forward svc/kps-grafana 3000:80
```

The **Oikade home** dashboard shows temperature and humidity per room, light and door timelines, motion events, battery levels, availability and automation runs. On a Pi 4, consider a Prometheus memory limit and an SSD for the Prometheus PVC.

## 15. Back up what Git can't rebuild

Everything in your home repo can be rebuilt from Git. These can't:

| What | Where | If you lose it |
|---|---|---|
| Zigbee network identity | Secret `home/z2m-network` | Every Zigbee device must be paired again |
| HomeKit identity | Secret `home/main-identity` | The Home app sees a new bridge |
| HomeKit pairings and keys | PVC `home/main-hap` | You must re-pair, and the Home app forgets rooms, scenes and automations |
| Zigbee database and coordinator backup | PVC `home/z2m-data` | zigbee2mqtt rebuilds what it can; devices may need re-interviewing |
| Plugin credentials | Your `spec.secretRef` Secrets, such as `home/sonoff-keys` | The plugin reports an error for every device. If they're in Git with SOPS, only the age key needs backing up. |

Export the Secrets now, and again whenever you create a new Bridge or Zigbee2MQTT instance. Store them somewhere safe, such as a password manager. Don't put them in Git unencrypted.

```sh
kubectl -n home get secret z2m-network main-identity -o yaml > oikade-secrets-backup.yaml
```

For the PVCs on `local-path`, copy the directories off the Pi they live on. Find which Pi holds each one, then list its directories, which are named `pvc-<uid>_<namespace>_<claim>`:

```sh
kubectl -n home get pod -o wide                       # main-hap and z2m pods show their node
ssh pi-1 sudo ls /var/lib/rancher/k3s/storage/        # e.g. pvc-3f2..._home_main-hap
```

Copy them to another machine from a nightly cron, e.g. `rsync -a --rsync-path="sudo rsync" pi-1:/var/lib/rancher/k3s/storage/pvc-3f2..._home_main-hap/ backups/main-hap/`. If you use Longhorn, use its scheduled backups to NFS or S3 instead.

**To restore:** apply the Secrets *before* the Bridge and Zigbee2MQTT objects. The operator never overwrites existing ones, so it picks them up.

## 16. Upgrades

```sh
make docker-buildx IMG=ghcr.io/<github-user>/oikade:v0.2.0 PLATFORMS=linux/arm64
make build-installer IMG=ghcr.io/<github-user>/oikade:v0.2.0
kubectl apply -f dist/install.yaml        # or commit dist/install.yaml and let Flux apply it
```

The operator and runtime roll over. HAP pods pick up the new image the next time the operator applies them, keeping their pairings. They use the Recreate strategy, so expect a few seconds when HomeKit shows "No Response".

To upgrade Zigbee2MQTT, set `spec.image` on the Zigbee2MQTT object, or change the operator's `--zigbee2mqtt-image` default.

## 17. Troubleshooting

| Symptom | Check |
|---|---|
| Bridge doesn't appear in the Home app | `dns-sd -B _hap._tcp` from a Mac on the same network. If it's missing: is the HAP pod on the labelled node (`kubectl -n home get pod -o wide`)? Same subnet as the iPhone? Is UDP 5353 open? Does the router or access point block multicast between Wi-Fi and Ethernet? |
| "Accessory not found" or pairing fails | `kubectl -n home logs deploy/main-hap`. A port conflict on the node shows up there. |
| Device stays `Resolved=False` (`NotPaired`) | The IEEE address in Git doesn't match the paired device. Compare with the Zigbee2MQTT frontend. |
| Device stays `Resolved=False` (`NoDriver`) | `instanceRef` names a Zigbee2MQTT that doesn't exist, or another Device already claims that IEEE address (see the Zigbee2MQTT `Ready` condition). |
| Device stays `Resolved=False` (`PluginError`) | The plugin rejected the Device's `parameters` or has no key for its id. The condition message says which. |
| Plugin isn't `READY` | `kubectl -n home get plugin <name> -o yaml` shows the reason: `SecretNotFound`, `DuplicateDevice`, `NameConflict`, or `Progressing` while the pod starts. Check `kubectl -n home logs deploy/<name>-plugin`. |
| A capability is missing | Oikade doesn't map that expose yet, e.g. colour, colour temperature or multi-gang switches (`state_l2`). |
| HomeKit shows "No Response" | Is the device `Available=False` in `kubectl get devices`? Does `kubectl -n home logs deploy/main-hap` show `Failed to set capability`? |
| Automation never fires | Is it `READY` in `kubectl get automations`? `oikade_automation_runs_total` shows whether it's hitting `condition_false`. Check `--timezone` if the condition uses the time. |
| zigbee2mqtt crash-loops | `kubectl -n home logs deploy/z2m`. Check the coordinator address or USB path, and that `adapter` matches the coordinator's chip. |
| Something is wrong with MQTT | Watch the bus: `kubectl -n home run mqtt --rm -it --image=eclipse-mosquitto:2.1.2-alpine -- mosquitto_sub -h oikade-mosquitto.oikade-system.svc -t '#' -v` |
