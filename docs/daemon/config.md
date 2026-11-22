# Device config: config.json

A host's `config.json` describes the devices it serves: which ones a hub
shares, each one's description, and each one's
[traffic-shaping](network.md) calls. Nothing else writes it — there is no
API to change it — and a device listing returns what it says.

```json
{
  "hubDevices": [
    {
      "id": "00008110-EXAMPLE0001",
      "description": "Lab iPhone 13 Pro, landscape stand",
      "trafficShaping": {
        "setTrafficShape": {
          "method": "POST",
          "url": "https://shaper.example/v1/shape",
          "headers": { "Authorization": "Bearer EXAMPLE-TOKEN" },
          "body": { "ip": "192.0.2.15", "mbit": "${mbit}", "lossPct": "${lossPct}",
                    "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}" }
        },
        "resetTrafficShape": {
          "method": "DELETE",
          "url": "https://shaper.example/v1/shape/192.0.2.15",
          "headers": { "Authorization": "Bearer EXAMPLE-TOKEN" }
        }
      }
    },
    { "id": "EXAMPLEPIXEL01", "description": "Spare Pixel 8 on the left rack" },
    { "id": "avd:ci-pixel" }
  ]
}
```

## Where it is read

| | |
| --- | --- |
| `--config FILE` on `serve`, `hub start` and `hub service install` (or `MAESTRO_D_CONFIG`) | That file, as `config.json`, and nothing else. A file that does not exist is `USAGE` when the daemon starts |
| neither flag | `<home>/config.json` (`~/.maestro-d/config.json`, or under `$MAESTRO_D_HOME`) when it exists, else `<home>/network.json` |
| `--network-config FILE` (or `MAESTRO_D_NETWORK_CONFIG`) | Deprecated: that file, in the older [network.json](network.md#the-older-networkjson) format |

Only one file is ever read: `network.json` is read only when there is no
`config.json`, and `--config` with `--network-config` is `USAGE`.
`hub service install` passes the flag on to the LaunchAgent. The daemon
logs the file it reads when it starts, and warns about any other config
file that this order leaves unread.

- It must be readable by its owner only (`chmod 600`), because a shaping
  call may carry credentials. On Windows the file's access control list
  governs instead.
- A missing `<home>` file, a missing `--network-config` file, or a `--config`
  file removed while the daemon runs is an empty config.
- A malformed file, an unknown key, an entry with no `id`, an id listed
  twice, or `trafficShaping` without both of its calls is an error naming the
  file, the device and the problem: `hub start` refuses to start, shaping
  commands answer `USAGE`, and device listings log it and carry no
  descriptions or shaping.

## hubDevices

Each entry is one device of this host:

| Key | | Meaning |
| --- | --- | --- |
| `id` | required | The device's id, as `maestro-d devices` lists it: a UDID, an adb serial or `avd:<name>`. `avd:<name>` also names that AVD while it runs under an emulator serial, and an Android boot or create of that bare name (a boot with no platform is Android when only an AVD has the name). A device's name does not match, as it does for `--share-device` |
| `description` | optional | Free text, returned as `description` |
| `trafficShaping` | optional | The device's `setTrafficShape` and `resetTrafficShape` calls, both required when the key is there ([the calls](network.md#a-call)) |

### Sharing

**A hub shares exactly the devices it names**: every `hubDevices` entry, plus
`--share-device` and `--share-tv` on its command line. A device named nowhere
is private to its machine, and a hub that names nothing shares nothing. An
Apple TV listed in `hubDevices` is shared like one named with `--share-tv`.
Two exceptions: a cloud device the hub launches for a lease is always in its
pool, and the [computer device](../macos.md) (`mac`) never is, even when
`hubDevices` lists it; its entry still gives it a description.

The hub reads `hubDevices` when it starts, so a change to which devices it
shares takes a restart (`hub stop` and `hub start`, or the service's
kickstart). A daemon that is not a hub reads no share list: its remote
callers see every device, as before.

### Descriptions and shaping

These are read on every call and every listing, so an edit to them needs
no restart. Device listings, `GET /v1/devices/{id}`, and the attach, lease,
boot and create answers carry `description`, and `trafficShaping: true` for
a device with both calls; a hub passes both on to every hub in its mesh. A
blank description is left out. A running emulator takes its description
and its calls from its serial's entry, and each one the serial's entry
lacks from its AVD's `avd:<name>` entry. A device without `trafficShaping`
in either answers `setTrafficShape` and `resetTrafficShape` with
`UNSUPPORTED`.
