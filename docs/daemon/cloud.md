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

## Providers

A cloud provider implements one Go interface, `cloudprov.RemoteProvider`
(`pkg/cloudprov`): it lists models, allocates a session for a real device
and waits until it runs, installs apps into it, and releases it. The daemon
does everything the providers share: it holds the device's Appium driver,
keeps the session alive, releases it when idle, binds it to a lease, and
records it so a crashed daemon's sessions are still released. AWS Device
Farm is `pkg/devicefarm`; BrowserStack will be the second provider on the
same interface.
