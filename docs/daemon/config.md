# Device config: config.json

A host's `config.json` describes the devices it serves: which ones a hub
shares, each one's description and capabilities, and each one's
[traffic-shaping](network.md) calls; the keys its [AI checks](#ai)
spend; and the hosts [`installApp`](#installapp) may download a build from.
Device listings include descriptions, capabilities and shaping support.

```json
{
  "hubDevices": [
    {
      "id": "00008110-EXAMPLE0001",
      "description": "Lab iPhone 13 Pro, landscape stand",
      "capabilities": { "example": { "class": "ios-phone", "features": ["camera"] } },
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
  ],
  "ai": {
    "jevKeyFile": "jev.key",
    "anthropicKeyFile": "anthropic.key",
    "maxBudgetUsdPerCall": 0.5
  },
  "installApp": {
    "allowHosts": ["mason.teamstation.app"]
  }
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
| `capabilities` | optional | Any JSON value, returned as `capabilities` exactly as written (key order, number literals and all). maestro-d gives it no meaning: each installation defines its own convention, and a test runner matches devices against it |
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
boot and create answers carry `description`, `capabilities`, and
`trafficShaping: true` for a device with both calls; a hub passes them on,
unchanged, to every hub in its mesh. A blank description and a `null`
capabilities are left out. A running emulator takes its description
and its calls from its serial's entry, and each one the serial's entry
lacks from its AVD's `avd:<name>` entry. Its capabilities come the same
way: its serial's entry's, else its AVD's. A device without `trafficShaping`
in either answers `setTrafficShape` and `resetTrafficShape` with
`UNSUPPORTED`.

## ai

The optional top-level `ai` block holds the keys this daemon's
`assertWithAI` and `assertNoDefectsWithAI` spend ([the AI checks](rest.md#ai-checks)).
Any daemon reads it: a hub for the devices it owns, and a plain local
daemon (a browser's, for instance) for its own sessions.

| Key | | Meaning |
| --- | --- | --- |
| `jevKeyFile` | one of the two | A file holding the TypeSafe Jev API key |
| `anthropicKeyFile` | one of the two | A file holding the Anthropic API key, for Claude Haiku, Sonnet and Opus |
| `maxBudgetUsdPerCall` | optional | The most one check may spend on these keys, in US dollars. A client's larger `budgetUsd` is capped to it, and it is the budget of a check that names none (0 or absent: no cap) |

- A relative key file is relative to the config file's directory. Each
  must be readable by its owner only (`chmod 600`), and holds the key alone
  (surrounding whitespace is ignored). The files are read at each check, so
  a new key needs no restart; their content never reaches a log, a report
  or an answer.
- An unknown key inside `ai`, a block that names neither file, or a
  negative cap is an error, as for `hubDevices`.

**Which keys a check spends.** A daemon with an `ai` block spends its own
keys for every device it owns and ignores the keys an attach sends, so a
hub's devices are judged on the hub owner's account. A daemon without one
spends the keys its session's attach carried (`attach --ai-keys-file`,
REST and JavaScript `ai: {jevKey, anthropicKey}`). Attach keys stay in the
session for the AI checks only: they are not session variables, never
written to reports or logs, never echoed in an answer, and a hub never
forwards them to the hub that owns the device. A stage whose key the
daemon has neither way is skipped (`no-key` in the result's
`escalations`); with no key at all the verdict is `unsure`.

## installApp

The optional top-level `installApp` block names the hosts this daemon
downloads a build from when an install names a `url`
([installing from a URL](rest.md#apps)). On a hub that is the hub the device
is plugged into: a hub routing the install on to it does not fetch.

| Key | | Meaning |
| --- | --- | --- |
| `allowHosts` | required | Host names or IP addresses, without scheme, port, path or wildcard (`mason.teamstation.app`, `bern`, `192.0.2.20`), matched case-insensitively on the URL's host whatever its port |

- **With the block**, every caller, local or remote, may download only from
  a listed host. A redirect must stay on the listed hosts, with one
  exception: a redirect over https to a DigitalOcean Space
  (`*.digitaloceanspaces.com`), where Mason sends its signed download. Any
  other host or redirect answers `FORBIDDEN` (with `details.field` `url`),
  before any request to it.
- **Without the block**, a local caller (the unix socket, or loopback on a
  plain `serve --http`) may name any URL and follow any redirect, as before.
  A remote caller (any TCP request to a hub, and every install a hub
  routes on to the hub that owns the device) is refused with `FORBIDDEN`,
  so a client of a hub cannot make it fetch an arbitrary URL. A hub whose
  devices are installed from Mason through the mesh lists Mason's host.
- The block is read at each install, so a new host needs no restart. An
  unknown key, a block with no host, or a host given with a scheme, port,
  path or wildcard is an error, as for `hubDevices`; a download while the
  file has one answers `INTERNAL`.
