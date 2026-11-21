# AWS Device Farm

maestro-d launches real phones on [AWS Device Farm](https://aws.amazon.com/device-farm/)
remote access sessions and drives them through Device Farm's managed
Appium endpoint. How cloud devices are listed, launched and released is in
[docs/daemon/cloud.md](../daemon/cloud.md); this page is what is specific
to Device Farm.

## Credentials

Create an IAM user for maestro-d with this policy (or attach
`AWSDeviceFarmFullAccess`), and an access key for it:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "MaestroDDeviceFarm",
    "Effect": "Allow",
    "Action": [
      "devicefarm:GetAccountSettings", "devicefarm:ListDevices", "devicefarm:GetDevice",
      "devicefarm:ListProjects", "devicefarm:CreateProject",
      "devicefarm:CreateUpload", "devicefarm:GetUpload", "devicefarm:ListUploads", "devicefarm:DeleteUpload",
      "devicefarm:CreateRemoteAccessSession", "devicefarm:GetRemoteAccessSession",
      "devicefarm:StopRemoteAccessSession", "devicefarm:ListRemoteAccessSessions",
      "devicefarm:InstallToRemoteAccessSession", "devicefarm:ListArtifacts"
    ],
    "Resource": "*"
  }]
}
```

A daemon reads the standard AWS chain from its own environment:
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or
`~/.aws/credentials` and `~/.aws/config`. It reads them when it starts, so
after you set them run `maestro-d stop`, and the next command starts a
daemon that has them. The region is always `us-west-2`, Device Farm's only
one.

The sessions and uploads go in a Device Farm project named `maestro-d`,
created on first use; `MAESTRO_D_DEVICEFARM_PROJECT` names another project
or gives a project ARN.

## The catalog

`maestro-d device catalog --location cloud` lists the models Device Farm
offers for remote access (about 180: iOS 15 to 27, Android 9 to 17), with
`--platform`, `--name`, `--os-min`, `--os-max` and `--available` to narrow
them. A model's id is `df:<platform>:<name>:<os>`; launch it by that id or
by name.

## Launch, drive, release

```sh
maestro-d device create --provider devicefarm --platform ios --name 'Apple iPhone 15' \
  --app BStackSampleApp.ipa --app-id com.browserstack.Sample-iOS --idle-release 30m
maestro-d launchApp com.browserstack.Sample-iOS --device df-2fd5de78
maestro-d run flow.yaml --device df-2fd5de78
maestro-d device delete df-2fd5de78
```

A launch takes about a minute (Device Farm prepares the phone). The app is
uploaded once, under a name made from its content hash, and reused for 30
days; `.apk` and `.ipa` up to 4 GB. Public iOS devices re-sign the app with
Device Farm's own profile.
iOS 27 and later run Appium 3 (`--appium-version` overrides; earlier iOS
and Android run Appium 2).

## Through a hub

`maestro-d hub start --devicefarm --devicefarm-credentials-file F
[--devicefarm-profile P] [--devicefarm-project NAME|ARN]
[--cloud-max-devices N]` gives a hub the credentials; any client of the
mesh then leases cloud devices (`lease --location cloud`). The credentials
file must be a regular file with mode 0600 or stricter, and `hub service
install` records its path, never the keys. At start the hub reads the
account once (`cloud: devicefarm ready (trial minutes left N)` in its log)
and refuses to start when that fails.

## Limits

- **Session length:** 150 minutes; maestro-d releases the device 30 s before.
- **Idle:** Device Farm ends a session after 5 minutes without a command;
  maestro-d pings it every minute while it is idle, and releases it after
  its own idle timeout (default 10 minutes).
- **Concurrency:** 2 metered remote access sessions per account by
  default (Service Quotas `L-F73D98D4`). A launch waits up to
  `--boot-timeout` (default 900 s) for a slot, then fails `BUSY`.
- **Billing:** metered at $0.17 per device minute, setup and teardown
  included; a new account has 1,000 free trial minutes. Every release
  reports its minutes, and `maestro-d devicefarm sessions` lists the billed
  minutes of every session.

## What does not work

Device Farm's managed Appium endpoint refuses some commands, and a cloud
device has no host-side screen capture:

- `clearState` (Device Farm blocks `mobile: clearApp`; on Android the
  driver's `pm clear` fallback works only where Device Farm allows a shell);
- `setPermissions` on iOS;
- `startRecording`, `addMedia`, screen streams (`CAPTURE_UNAVAILABLE`,
  `unsupported-platform`), biometrics;
- uninstalling an app during a session (release the device instead);
- the steps the Appium driver never supports: airplane mode, dark mode,
  `openNotifications`, `clearKeychain`, `travel`.

## Troubleshooting

| Error | Cause |
| --- | --- |
| `USAGE` no AWS credentials for Device Farm … | the daemon started without credentials: set them and `maestro-d stop` |
| `USAGE` … not authorized to perform: devicefarm:… | the IAM policy lacks the action it names |
| `BUSY` … waited … in PENDING_CONCURRENCY | the account's sessions are all in use: release another device or raise the quota |
| `DEVICE_NOT_FOUND` … closest: … | no model matches `--name` and the OS range; the message names the closest |
| `DEVICE_ERROR` cloud device … is gone: … | the session ended (idle, the cap, or Device Farm ended it); launch another |
