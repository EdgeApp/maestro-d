# Device config: config.json

A host's `config.json` describes the devices it serves: which ones a hub
shares, each one's description, and each one's
[traffic-shaping](network.md) calls. Device listings include descriptions and shaping support.

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

Use `--config FILE` or `MAESTRO_D_CONFIG` to select the file. By default,
maestro-d reads `<home>/config.json`, under `MAESTRO_D_HOME` or
`~/.maestro-d`. A hub service uses the file selected at installation.
An explicitly selected file must exist at startup.

To migrate from 3.x, move each entry from `network.json` into
`hubDevices`. Set its `id` to the old device key and copy both calls into
`trafficShaping`. Move devices previously shared with `--share-device` or
`--share-tv` into the same array using their stable IDs. Remove those flags
and replace `--network-config` or `MAESTRO_D_NETWORK_CONFIG` with
`--config` or `MAESTRO_D_CONFIG` if you need a custom path.

- It must be readable by its owner only (`chmod 600`), because a shaping
  call may carry credentials. On Windows the file's access control list
  governs instead.
- A missing default file, or a `--config`
  file removed while the daemon runs is an empty config.
- A malformed file, an unknown key, an entry with no `id`, an id listed
  twice, or `trafficShaping` without both of its calls is an error naming the
  file, the device and the problem: `hub start` refuses to start, shaping
  commands on the local daemon answer `USAGE`. A running hub keeps serving
  its last valid config until the file is repaired.

## hubDevices

Each entry is one device of this host:

| Key | | Meaning |
| --- | --- | --- |
| `id` | required | The device's id, as `maestro-d devices` lists it: a UDID, an adb serial or `avd:<name>`. `avd:<name>` also names that AVD while it runs under an emulator serial, and an Android boot or create of that bare name (a boot with no platform is Android when only an AVD has the name) |
| `description` | optional | Free text, returned as `description` |
| `trafficShaping` | optional | The device's `setTrafficShape` and `resetTrafficShape` calls, both required when the key is there ([the calls](network.md#a-call)) |

### Sharing

List each shared device in `hubDevices`, including Apple TVs. An empty
list keeps all local devices private. A cloud device launched for a lease
joins the pool for that lease. The computer device (`mac`) stays private,
even when its entry supplies a local description.

The local daemon uses the file for descriptions and shaping. Local commands
can still use any device on this computer.

### Editing the shared devices

On the device host, run:

```sh
maestro-d-hub devices --json
maestro-d-hub devices add "$UDID" --description "Rack 2"
maestro-d-hub devices set "$UDID" --description "Camera bench"
maestro-d-hub devices remove "$UDID"
```

The commands use the running hub's Unix socket and update the active file
immediately. When the hub is stopped, the CLI locks the home and edits
the file directly. Use `--config FILE` for a custom path. A running hub
checks that the requested path matches its own active config.

You can configure a device before connecting it. The listing's `listed`
field tells you whether discovery sees it. Removal reports the holder and
time window of each live lease or reservation. To end them and remove the
device in one operation, pass `--end-leases` to `devices remove`.
An ordinary hand edit that removes a device keeps existing grants usable
by their holders until their windows end. New clients see the updated pool.

The hub checks hand edits at most once a second. While a file is invalid,
the hub retains its last valid list and reports the error in `configError`
on the local config listing. Repairing the file clears that error.

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
