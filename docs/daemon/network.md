# Traffic shaping

maestro-d can degrade a device's network — cap its bandwidth, drop packets,
add latency and jitter — by calling a traffic shaper your lab already runs.
maestro-d knows nothing about that shaper. Each host is configured, per
device, with the HTTP call that sets a shape and the call that removes it,
and `setTrafficShape` / `resetTrafficShape` make exactly that call.

A device with no entry cannot be shaped, and otherwise works as before.

## The config file

`<home>/network.json`: `~/.maestro-d/network.json`, or
`$MAESTRO_D_HOME/network.json` when that is set. `--network-config FILE` on
`serve` and `hub start` (or `MAESTRO_D_NETWORK_CONFIG`) points elsewhere;
`hub service install` passes it on to the LaunchAgent.

- It is read on **every** call and every device listing, so an edit needs no
  restart.
- It must be readable by its owner only (`chmod 600`), because a call may
  carry credentials. Otherwise every shaping command answers `USAGE`. On
  Windows, whose files have no such mode, the file's access control list
  governs instead.
- A missing file means no device is configured.
- A malformed file, an unknown key or placeholder, or a device with only one
  of its two calls answers `USAGE`, naming the file, the device and the
  problem. Device listings log the problem and report no device as
  shapeable.

```json
{
  "devices": {
    "00008110-EXAMPLE0001": {
      "setTrafficShape": {
        "method": "POST",
        "url": "https://shaper.example/v1/shape",
        "headers": { "Authorization": "Bearer EXAMPLE-TOKEN" },
        "body": { "ip": "192.0.2.15", "mbit": "${mbit}", "lossPct": "${lossPct}",
                  "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}" },
        "directionValues": { "download": "down", "upload": "up", "both": "both" },
        "insecureSkipVerify": true
      },
      "resetTrafficShape": {
        "method": "DELETE",
        "url": "https://shaper.example/v1/shape/192.0.2.15",
        "headers": { "Authorization": "Bearer EXAMPLE-TOKEN" },
        "insecureSkipVerify": true
      }
    },
    "EXAMPLEPIXEL01": {
      "setTrafficShape": {
        "method": "PUT",
        "url": "http://other-shaper.example/devices/7?mbit=${mbit}&loss=${lossPct}&delay=${latencyMs}&jitter=${jitterMs}&dir=${direction}"
      },
      "resetTrafficShape": { "method": "DELETE", "url": "http://other-shaper.example/devices/7" }
    }
  }
}
```

Devices are keyed by id, as `maestro-d devices` lists them: a UDID, an adb
serial or `avd:<name>`. Anything device-specific the shaper needs — an IP
address, its own device number — is written into that device's calls;
maestro-d never looks anything up.

### A call

| Key | | Meaning |
| --- | --- | --- |
| `method` | required | `GET`, `POST`, `PUT`, `PATCH` or `DELETE` |
| `url` | required | An absolute `http://` or `https://` URL |
| `headers` | optional | Header names and values |
| `body` | optional | Any JSON value, sent as `Content-Type: application/json`. Not on `GET` or `DELETE` |
| `directionValues` | optional, `setTrafficShape` only | Your shaper's word for `download`, `upload` and `both` |
| `insecureSkipVerify` | optional | Accept a self-signed certificate |

### Placeholders

maestro-d fills six placeholders, in the URL's path or query string, a
header value or the body. Any other `${…}`, or one in the URL's host, is a
config error.

| Placeholder | Value | Default |
| --- | --- | --- |
| `${mbit}` | bandwidth cap in megabits per second; `0` = no cap | `0` |
| `${lossPct}` | random packet loss, 0–100 | `0` |
| `${latencyMs}` | added one-way latency in milliseconds | `0` |
| `${jitterMs}` | latency jitter in milliseconds | `0` |
| `${direction}` | `download`, `upload` or `both`, or its `directionValues` word | `both` |
| `${deviceId}` | the device's id | — |

- **Every placeholder is always filled.** A value the command does not give
  takes its default, so every `setTrafficShape` sets the device's whole
  shape: a value left out is reset, not kept.
- **Typed body values:** a JSON string that is exactly one placeholder,
  `"${mbit}"`, becomes the value with its own JSON type — the shaper
  receives `"mbit": 1.5`, not `"1.5"`. Inside a longer string, a header or
  the URL, the value is text; in the URL it is path-escaped in the path
  and query-escaped in the query string.
- A number written in a body is sent exactly as written.
- Numbers are written in their shortest form: `1.5`, `0`, `100`.

## Commands

| Command | Fields | Configured device | Unconfigured device |
| --- | --- | --- | --- |
| `setTrafficShape` | `mbit`, `lossPct` (0–100), `latencyMs`, `jitterMs`: numbers ≥ 0, each optional, default 0. `direction`: `download`, `upload` or `both`, default `both`; it may be a `${VAR}` | One call, with every placeholder filled | `UNSUPPORTED`: "no traffic shaper is configured for <id> on this host" |
| `resetTrafficShape` | — | One call | `UNSUPPORTED` |

```sh
maestro-d setTrafficShape --device EXAMPLEPIXEL01 --mbit 1.5 --latencyMs 100
maestro-d setTrafficShape --device EXAMPLEPIXEL01 --lossPct 100 --direction upload
maestro-d resetTrafficShape --device EXAMPLEPIXEL01
```

```js
await device.setTrafficShape({ mbit: 1, latencyMs: 150, jitterMs: 30 })
await device.resetTrafficShape()
```

```yaml
- setTrafficShape:
    latencyMs: 250
    direction: download
```

- Different upload and download shapes take two calls, one per direction.
  What the second does to the other direction is up to the shaper:
  maestro-d sends one call per command and nothing else.
- A negative number, `lossPct` over 100, an unknown direction or an
  unknown field is `USAGE`, before any call is made.
- They run in a daemon session: a one-shot command, REST, the JS client or
  a flow sent to the daemon. `maestro-d test` does not shape traffic, and
  answers both with `UNSUPPORTED`.
- **The answer:** any `2xx` succeeds, and the step's `data` carries the
  status. Anything else, a refused connection, or no answer within 10 s
  fails the step with `COMMAND_FAILED`, the status and the first 2 KB of the
  answer. Redirects are not followed: a `3xx` fails like any other answer.

## Where the call is made

On the host the device is plugged into, with that host's config. Through a
[hub](hub.md), the command is routed to the owning hub under the device's
lease like any other device command, so a lease is all a caller needs.
Remote callers never see the config.

`GET /v1/devices` (and `device list --json`) shows `trafficShaping: true`
for a device whose host has both calls configured; a hub's merged listing
carries each hub's own answer.

## Simulating a lost network on iOS

iOS has no airplane mode in maestro-d: `setAirplaneMode` and
`toggleAirplaneMode` answer `UNSUPPORTED` on every iOS and tvOS device,
because turning the radios off would break the driver's own connection.
Drop every packet instead:

```sh
maestro-d setTrafficShape --device 00008110-EXAMPLE0001 --lossPct 100
```

WebDriverAgent keeps working, because it talks to a physical device over
USB. Android keeps its real airplane mode (adb).

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `UNSUPPORTED: no traffic shaper is configured for <id> on this host` | No entry for that exact id in this host's file, or no file. Check `maestro-d devices` for the id and which host owns the device |
| `USAGE: network config …: mode 0644 is readable by others` | `chmod 600` the file |
| `USAGE: network config …: device <id>: …` | That device's calls fail validation; the message names the key |
| `COMMAND_FAILED: setTrafficShape: POST … answered 4xx/5xx: …` | The shaper refused the call; its answer follows |
| The call succeeds but nothing changes on the device | The shaper accepted it but does not see the device's traffic — check the shaper's own view of the device |

Every call is logged in the daemon log (`<home>/run/<daemon>/daemon.log`)
with the device, the command, the method, the URL, the status and the
time. Header values, any user-info in the URL and its query string are
written as `[redacted]`, in the log and in a failed step's message; bodies
are not logged.
