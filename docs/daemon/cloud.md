# Cloud devices

maestro-d drives real phones in a device cloud the same way it drives the
phones, simulators and emulators on your own machines. AWS Device Farm is
the first cloud provider; its setup is in
[cloud-providers/devicefarm.md](../cloud-providers/devicefarm.md).

## Where a device is

Every device listing (`maestro-d device list`, `GET /v1/devices`, JS
`md.devices()`) says where each device is, relative to the computer the
request comes from:

| `location` | the device |
| --- | --- |
| `local` | is attached to, or runs on, your computer |
| `mesh` | is on another computer in the hub mesh, named by `machine` |
| `cloud` | is held by a cloud provider, named by `provider` (and by `hub` when a hub holds it) |

A device with a `provider` is `cloud`. Any other device is `local` when its
`machine` is your machine and `mesh` otherwise. Every daemon sets `machine`
to its host name; names compare case-insensitively, ignoring a trailing
`.local`.

Your machine is the `X-Maestro-Client-Machine` header, which the CLI, the
Go client and the JavaScript library always send (the host name). A
request without it, from `curl` say, is on the daemon's own machine when it
arrives over the unix socket or from a loopback address, and from an
unknown machine otherwise, which sees every non-cloud device as `mesh`. In
a mesh the hub you ask computes `location` after merging every hub's list,
so it is relative to you wherever the device is.

```
$ maestro-d device list --target-hub=lab-mac
ID                       PLATFORM KIND       STATE      ATTACHED LEASE                        LOCATION                 PROVIDER    NAME
FAKE-IOS-UDID-0001@lab-mac ios      device     connected  -        available                    local                    -           iPhone 13 Pro 26.1
FAKEPIXEL0003@lab-mini   android  device     device     -        available                    mesh (lab-mini)          -           Pixel 10 16
df-3f2a9c1e@lab-mac      android  cloud      allocated  yes      ci (28m)                     cloud (devicefarm@lab-mac) devicefarm Google Pixel 8 14
```

## Filters

The same filters apply to every device, cloud devices included:

| CLI | REST / JS | keeps |
| --- | --- | --- |
| `--location local,mesh,cloud` | `location` | these locations |
| `--provider devicefarm` | `provider` | that provider's devices |
| `--platform ios` | `platform` | that platform |
| `--kind cloud` | `kind` | device, emulator, simulator, tv or cloud |
| `--name 'pixel 8'` | `name` | a case-insensitive regular expression on the name and the id |
| `--os-min 16 --os-max 18` | `osVersionMin`, `osVersionMax` | the OS version range, inclusive |
| `--available` | `available` | devices a lease or attach could take now |

Versions compare as numbers, component by component, a missing component
counting as 0. The maximum compares at its own precision: `--os-max 17`
admits 17.5, and `--os-max 17.2` does not admit 17.4. A device with no
`osVersion` fails either bound. An unknown location, an invalid regular
expression or a version that is not digits and dots is `USAGE`.

## The cloud catalog

`maestro-d device catalog --location cloud` (`GET
/v1/catalog?location=cloud`, JS `md.catalog({location: 'cloud'})`) lists the
models a cloud provider can launch, filtered by `--provider`, `--platform`,
`--name`, `--os-min`, `--os-max` and `--available` (only models available
now):

```
$ maestro-d device catalog --location cloud --platform ios --os-min 27
PROVIDER    HUB            ID                                       PLATFORM NAME                         OS       FORM    AVAILABILITY
devicefarm  -              df:ios:apple-iphone-14:27.0              ios      Apple iPhone 14              27.0     phone   HIGHLY_AVAILABLE
devicefarm  -              df:ios:apple-iphone-18-pro:27.0          ios      Apple iPhone 18 Pro          27.0     phone   HIGHLY_AVAILABLE
```

A model's id is `df:<platform>:<name>:<os>`. Without `--location cloud` the
catalog lists what `device create` can make on this machine, as before, and
asks no cloud provider.

## Which hub launches

A cloud device is held by one daemon: your own (direct mode), or a hub
that holds the provider's credentials (hub mode). In a mesh, the catalog
and cloud launches go to:

1. the hub you name (`--hub`, `hub` in REST and JS);
2. otherwise the hub you are connected to, when it has the credentials;
3. otherwise the first linked hub, by name, that has them.

Each hub announces its cloud providers to its peers when their link opens,
and `GET /v1/hub` shows them in each peer's `cloud`. When no hub in the
mesh has credentials, the request fails `USAGE`: "no hub in this mesh has
Device Farm credentials".

`GET /v1/status` and `GET /v1/hub` list the providers a daemon or hub
launches on in `cloud`, with the number of cloud devices it holds (`held`)
and, on a hub, its cap (`maxDevices`).

## Direct mode: a cloud device on your own daemon

Your own daemon launches a cloud device with its own AWS credentials (the
environment, `AWS_PROFILE`, or `~/.aws/credentials`; the daemon reads them
when it starts, so start it, or `maestro-d stop` it, from a shell that has
them):

```
$ maestro-d device create --provider devicefarm --platform android --name 'Google Pixel 8' \
    --app WikipediaSample.apk --app-id org.wikipedia.alpha
df-c302205a launched: Google Pixel 8 15 (android, devicefarm); released after 600s idle, by 2026-11-21T03:15:24Z at the latest
$ maestro-d launchApp org.wikipedia.alpha --device df-c302205a
$ maestro-d run search.yaml --device df-c302205a
$ maestro-d device delete df-c302205a
df-c302205a deleted: Google Pixel 8 (android) (about 1.4 device minutes so far; Device Farm also bills its cleanup, so the billed figure is higher: devicefarm sessions shows it once the session completes)
```

The JavaScript library does the same with `md.createDevice({cloud: {…}})`,
and REST with `POST /v1/devices` and `create.cloud` ([rest.md](rest.md)).
Only a daemon's local callers launch cloud devices; a hub launches them for
leases.

The daemon holds the device's Appium driver, behind a loopback proxy, so
the provider's endpoint URL (itself a credential) never reaches a log, a
step result or a run file. Attach, one-shot commands, REST steps, flows and
`run` drive it as they drive any device, except what the provider does not
allow ([devicefarm.md](../cloud-providers/devicefarm.md#what-does-not-work)).

## Hub mode: cloud devices for every client

A hub started with `--devicefarm` holds Device Farm credentials (its own
environment, or `--devicefarm-credentials-file` / `--devicefarm-profile`)
and launches cloud devices for any client of any hub in its mesh, through
leases:

```
$ maestro-d hub start --name lab-mac --devicefarm --devicefarm-credentials-file ~/.aws/devicefarm-creds \
    --cloud-max-devices 2 --token-file … --peer-token-file …
$ maestro-d lease --target-hub=lab-mac.local --location cloud --platform ios --name 'Apple iPhone 15' --ttl 1h
df-2fd5de78@lab-mac leased: lease 5ae28c45-…@lab-mac, holder ci@laptop, expires … (59m59s; window 1h0m0s, max 2h30m0s)
$ maestro-d launchApp com.apple.Preferences --device df-2fd5de78@lab-mac
$ maestro-d release --device df-2fd5de78@lab-mac
df-2fd5de78@lab-mac released (lease 5ae28c45-…@lab-mac) (about 2.1 device minutes so far; …)
```

- **Only through leases.** On a hub, `device create` with a cloud provider
  is `FORBIDDEN` for every caller, with the hint to lease one, so every
  cloud device a hub holds belongs to exactly one lease.
- **Routing.** A cloud lease goes to the hub named with `--hub`, else the
  hub you ask when it holds the credentials, else the first linked hub by
  name that does ([Which hub launches](#which-hub-launches)). That hub
  tries once and its answer is final.
- **The cap.** A hub holds at most `--cloud-max-devices` cloud devices
  (default 2, Device Farm's default concurrency); one more lease is `BUSY`.
- **The window** is at most 150 minutes, the provider's session.
- **The lease and the device go together.** Releasing the lease, its
  expiry, or the hub stopping releases the device; the device's idle
  release or the session cap ends the lease (`lease.expired`, and later
  requests under it `LEASE_EXPIRED`, whose `details.hint` is a new cloud
  lease, since the device went with it; the CLI prints that hint and
  forgets the lease, and without a lease the device is `DEVICE_NOT_FOUND`).
- **Sharing.** A cloud device a hub launched is in its pool whatever
  `--share-device` and `config.json`'s `hubDevices` list, and is listed `available: false` while its lease
  lasts.
- **Apps** are installed after the lease, through the install route.

## Release

A cloud device costs money while it runs, so the daemon releases it:

- **When it is idle.** Any request that uses the device (a step, a flow, a
  screenshot, the hierarchy, state, vars, an attach, an install, a blob
  upload, a lease renewal) counts; reading its listing or `info` does not.
  A request still running holds the
  clock, and it restarts when the last one ends. After `--idle-release`
  (`idleReleaseSec`; default 10 minutes, 1 to 150) without one, the device
  is released. Until then the daemon pings the provider's session every
  minute, so the provider's own 5-minute idle limit never ends it. A held
  cloud device keeps its daemon running past the daemon's own idle timeout.
- **At the provider's cap.** Device Farm ends a session after 150 minutes;
  the daemon releases it 30 s before (`expiresAt`), and in the last 10
  minutes every step result warns `cloud session ends at …`.
- **When you stop or delete it**, and **when the daemon stops**. A release
  the provider refuses (a timeout, a 5xx) is retried in the background
  every 30 s until it succeeds, and the session stays in the cloud ledger
  meanwhile.
- **After a crash.** Every session is recorded in `<home>/run/cloud.json`
  the moment it is created, before the daemon waits for it. A daemon that
  starts under the name of one that crashed releases that one's sessions
  once it is serving; `maestro-d device stop --orphans` releases the
  sessions of every dead daemon, with this shell's AWS credentials or
  `--devicefarm-credentials-file` / `--devicefarm-profile`, and lists any it
  could not release with their ARNs. Both report each released session's
  minutes.

Every release reports the device minutes the session used
(`device.released` events, the stop and delete answers, `release`, `device
stop --orphans`). Device Farm settles its billed figure only once a session
has completed, and it bills the cleanup after the release too, which for
an iPhone can be ten minutes or more. Until then the minutes are counted
from the session's creation to the release, rounded down to a tenth, and
marked `estimated: true`, printed as "about N device minutes so far"; the
billed figure is higher by the cleanup.
`maestro-d devicefarm sessions` lists every session with its billed
minutes.

A session the provider ends on its own turns the device `state: ended`,
with `endedReason`, and later requests answer `DEVICE_ERROR` with that
reason until you delete it.

## Providers

A cloud provider implements one Go interface, `cloudprov.RemoteProvider`
(`pkg/cloudprov`): it lists models, allocates a session for a real device
and waits until it runs, installs apps into it, and releases it. The daemon
does everything the providers share: it holds the device's Appium driver,
keeps the session alive, releases it when idle, binds it to a lease, and
records it so a crashed daemon's sessions are still released. AWS Device
Farm is `pkg/devicefarm`; BrowserStack will be the second provider on the
same interface.
